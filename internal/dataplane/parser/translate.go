package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kong/go-kong/kong"
	"github.com/sirupsen/logrus"
	networkingv1 "k8s.io/api/networking/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"
	knative "knative.dev/networking/pkg/apis/networking/v1alpha1"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/dataplane/kongstate"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/util"
	configurationv1beta1 "github.com/kong/kubernetes-ingress-controller/v2/pkg/apis/configuration/v1beta1"
)

func serviceBackendPortToStr(port networkingv1.ServiceBackendPort) string {
	if port.Name != "" {
		return fmt.Sprintf("pname-%s", port.Name)
	}
	return fmt.Sprintf("pnum-%d", port.Number)
}

func fromIngressV1beta1(log logrus.FieldLogger, ingressList []*networkingv1beta1.Ingress, hashedRouteNames bool) ingressRules {
	result := newIngressRules()

	var allDefaultBackends []networkingv1beta1.Ingress
	sort.SliceStable(ingressList, func(i, j int) bool {
		return ingressList[i].CreationTimestamp.Before(
			&ingressList[j].CreationTimestamp)
	})

	for _, ingress := range ingressList {
		ingressSpec := ingress.Spec
		log = log.WithFields(logrus.Fields{
			"ingress_namespace": ingress.Namespace,
			"ingress_name":      ingress.Name,
		})

		if ingressSpec.Backend != nil {
			allDefaultBackends = append(allDefaultBackends, *ingress)
		}

		result.SecretNameToSNIs.addFromIngressV1beta1TLS(ingressSpec.TLS, ingress.Namespace)

		// a prefix that will be used for all kong.Routes derived from the ingress rules.
		routePrefix := fmt.Sprintf("%s.%s.%s.", IngressV1Beta1RoutePrefix, ingress.Namespace, ingress.Name)

		usedRouteNames := make(map[string]struct{})
		for i, rule := range ingressSpec.Rules {
			host := rule.Host
			if rule.HTTP == nil {
				continue
			}
			for j, rulePath := range rule.HTTP.Paths {
				path := rulePath.Path

				if strings.Contains(path, "//") {
					log.Errorf("rule skipped: invalid path: '%v'", path)
					continue
				}
				if path == "" {
					path = "/"
				}

				// by default if hashedRouteNames is not configured we'll use the legacy
				// convention for naming the route numerically in a sequence according to
				// the order it was configured within the Ingress spec. The downside of
				// this naming convention is that for use cases where the spec updates
				// often this has the potential to cause connection thrash as routes may
				// need to be replaced entirely to maintain the sequence.
				routeName := fmt.Sprintf("%s.%s.%d%d", ingress.Namespace, ingress.Name, i, j)

				// if hashed route names are requested, we use a unique hash
				// for the name instead of a numbered sequence.
				if hashedRouteNames {
					// in order to determine a unique name for the route to
					// be created we need the unique base ingress Rule with
					// the specific HTTP path that we're making the rule for,
					// as this is uniquely identifying information.
					uniqueRule := rule.DeepCopy()
					uniqueRule.HTTP.Paths = []networkingv1beta1.HTTPIngressPath{rulePath}

					// determine the unique name for the route to be created,
					// unique names help to avoid configuration thrash from
					// route names changing due to configuration ordering.
					var err error
					routeName, err = getUniqIDForRouteConfig(uniqueRule)
					if err != nil {
						log.Errorf("rule skipped: could not determine unique name: %w", err)
						continue
					}
					routeName = routePrefix + routeName

					// there's technically an infinitesmal chance of a collision,
					// if that happens add the rule order number to the name to
					// ensure uniqueness otherwise this would create a deadlock
					// on data-plane configurations.
					if _, exists := usedRouteNames[routeName]; exists {
						routeName = fmt.Sprintf("%s-%d-%d", routeName, i, j)
					}
					usedRouteNames[routeName] = struct{}{}
				}

				r := kongstate.Route{
					Ingress: util.FromK8sObject(ingress),
					Route: kong.Route{
						Name:              kong.String(routeName),
						Paths:             kong.StringSlice(path),
						StripPath:         kong.Bool(false),
						PreserveHost:      kong.Bool(true),
						Protocols:         kong.StringSlice("http", "https"),
						RegexPriority:     kong.Int(0),
						RequestBuffering:  kong.Bool(true),
						ResponseBuffering: kong.Bool(true),
					},
				}
				if host != "" {
					hosts := kong.StringSlice(host)
					r.Hosts = hosts
				}

				serviceName := ingress.Namespace + "." +
					rulePath.Backend.ServiceName + "." +
					rulePath.Backend.ServicePort.String()
				service, ok := result.ServiceNameToServices[serviceName]
				if !ok {
					service = kongstate.Service{
						Service: kong.Service{
							Name: kong.String(serviceName),
							Host: kong.String(rulePath.Backend.ServiceName +
								"." + ingress.Namespace + "." +
								rulePath.Backend.ServicePort.String() + ".svc"),
							Port:           kong.Int(DefaultHTTPPort),
							Protocol:       kong.String("http"),
							Path:           kong.String("/"),
							ConnectTimeout: kong.Int(DefaultServiceTimeout),
							ReadTimeout:    kong.Int(DefaultServiceTimeout),
							WriteTimeout:   kong.Int(DefaultServiceTimeout),
							Retries:        kong.Int(DefaultRetries),
						},
						Namespace: ingress.Namespace,
						Backend: kongstate.ServiceBackend{
							Name: rulePath.Backend.ServiceName,
							Port: PortDefFromIntStr(rulePath.Backend.ServicePort),
						},
					}
				}
				service.Routes = append(service.Routes, r)
				result.ServiceNameToServices[serviceName] = service
			}
		}
	}

	sort.SliceStable(allDefaultBackends, func(i, j int) bool {
		return allDefaultBackends[i].CreationTimestamp.Before(&allDefaultBackends[j].CreationTimestamp)
	})

	// Process the default backend
	if len(allDefaultBackends) > 0 {
		ingress := allDefaultBackends[0]
		defaultBackend := allDefaultBackends[0].Spec.Backend
		serviceName := allDefaultBackends[0].Namespace + "." +
			defaultBackend.ServiceName + "." +
			defaultBackend.ServicePort.String()
		service, ok := result.ServiceNameToServices[serviceName]
		if !ok {
			service = kongstate.Service{
				Service: kong.Service{
					Name: kong.String(serviceName),
					Host: kong.String(defaultBackend.ServiceName + "." +
						ingress.Namespace + "." +
						defaultBackend.ServicePort.String() + ".svc"),
					Port:           kong.Int(DefaultHTTPPort),
					Protocol:       kong.String("http"),
					ConnectTimeout: kong.Int(DefaultServiceTimeout),
					ReadTimeout:    kong.Int(DefaultServiceTimeout),
					WriteTimeout:   kong.Int(DefaultServiceTimeout),
					Retries:        kong.Int(DefaultRetries),
				},
				Namespace: ingress.Namespace,
				Backend: kongstate.ServiceBackend{
					Name: defaultBackend.ServiceName,
					Port: PortDefFromIntStr(defaultBackend.ServicePort),
				},
			}
		}
		r := kongstate.Route{
			Ingress: util.FromK8sObject(&ingress),
			Route: kong.Route{
				Name:              kong.String(ingress.Namespace + "." + ingress.Name),
				Paths:             kong.StringSlice("/"),
				StripPath:         kong.Bool(false),
				PreserveHost:      kong.Bool(true),
				Protocols:         kong.StringSlice("http", "https"),
				RegexPriority:     kong.Int(0),
				RequestBuffering:  kong.Bool(true),
				ResponseBuffering: kong.Bool(true),
			},
		}
		service.Routes = append(service.Routes, r)
		result.ServiceNameToServices[serviceName] = service
	}

	return result
}

func fromIngressV1(log logrus.FieldLogger, ingressList []*networkingv1.Ingress, hashedRouteNames bool) ingressRules {
	result := newIngressRules()

	var allDefaultBackends []networkingv1.Ingress
	sort.SliceStable(ingressList, func(i, j int) bool {
		return ingressList[i].CreationTimestamp.Before(
			&ingressList[j].CreationTimestamp)
	})

	for _, ingress := range ingressList {
		ingressSpec := ingress.Spec
		log = log.WithFields(logrus.Fields{
			"ingress_namespace": ingress.Namespace,
			"ingress_name":      ingress.Name,
		})

		if ingressSpec.DefaultBackend != nil {
			allDefaultBackends = append(allDefaultBackends, *ingress)
		}

		result.SecretNameToSNIs.addFromIngressV1TLS(ingressSpec.TLS, ingress.Namespace)

		// a prefix that will be used for all kong.Routes derived from the ingress rules.
		routePrefix := fmt.Sprintf("%s.%s.%s.", IngressV1RoutePrefix, ingress.Namespace, ingress.Name)

		usedRouteNames := make(map[string]struct{})
		for i, rule := range ingressSpec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for j, rulePath := range rule.HTTP.Paths {
				if strings.Contains(rulePath.Path, "//") {
					log.Errorf("rule skipped: invalid path: '%v'", rulePath.Path)
					continue
				}

				pathType := networkingv1.PathTypeImplementationSpecific
				if rulePath.PathType != nil {
					pathType = *rulePath.PathType
				}

				paths, err := pathsFromK8s(rulePath.Path, pathType)
				if err != nil {
					log.Errorf("rule skipped: pathsFromK8s: %v", err)
					continue
				}

				// by default if hashedRouteNames is not configured we'll use the legacy
				// convention for naming the route numerically in a sequence according to
				// the order it was configured within the Ingress spec. The downside of
				// this naming convention is that for use cases where the spec updates
				// often this has the potential to cause connection thrash as routes may
				// need to be replaced entirely to maintain the sequence.
				routeName := fmt.Sprintf("%s.%s.%d%d", ingress.Namespace, ingress.Name, i, j)

				// if hashed route names are requested, we use a unique hash
				// for the name instead of a numbered sequence.
				if hashedRouteNames {
					// in order to determine a unique name for the route to
					// be created we need the unique base ingress Rule with
					// the specific HTTP path that we're making the rule for,
					// as this is uniquely identifying information.
					uniqueRule := rule.DeepCopy()
					uniqueRule.HTTP.Paths = []networkingv1.HTTPIngressPath{rulePath}

					// determine the unique name for the route to be created,
					// unique names help to avoid configuration thrash from
					// route names changing due to configuration ordering.
					routeName, err = getUniqIDForRouteConfig(uniqueRule)
					if err != nil {
						log.Errorf("rule skipped: could not determine unique name: %w", err)
						continue
					}
					routeName = routePrefix + routeName

					// there's technically an infinitesmal chance of a collision,
					// if that happens add the rule order number to the name to
					// ensure uniqueness otherwise this would create a deadlock
					// on data-plane configurations.
					if _, exists := usedRouteNames[routeName]; exists {
						routeName = fmt.Sprintf("%s-%d-%d", routeName, i, j)
					}
					usedRouteNames[routeName] = struct{}{}
				}

				r := kongstate.Route{
					Ingress: util.FromK8sObject(ingress),
					Route: kong.Route{
						Name:              kong.String(routeName),
						Paths:             paths,
						StripPath:         kong.Bool(false),
						PreserveHost:      kong.Bool(true),
						Protocols:         kong.StringSlice("http", "https"),
						RegexPriority:     kong.Int(priorityForPath[pathType]),
						RequestBuffering:  kong.Bool(true),
						ResponseBuffering: kong.Bool(true),
					},
				}
				if rule.Host != "" {
					r.Hosts = kong.StringSlice(rule.Host)
				}

				port := PortDefFromServiceBackendPort(&rulePath.Backend.Service.Port)
				serviceName := fmt.Sprintf("%s.%s.%s", ingress.Namespace, rulePath.Backend.Service.Name,
					serviceBackendPortToStr(rulePath.Backend.Service.Port))
				service, ok := result.ServiceNameToServices[serviceName]
				if !ok {
					service = kongstate.Service{
						Service: kong.Service{
							Name: kong.String(serviceName),
							Host: kong.String(fmt.Sprintf("%s.%s.%s.svc", rulePath.Backend.Service.Name, ingress.Namespace,
								port.CanonicalString())),
							Port:           kong.Int(DefaultHTTPPort),
							Protocol:       kong.String("http"),
							Path:           kong.String("/"),
							ConnectTimeout: kong.Int(DefaultServiceTimeout),
							ReadTimeout:    kong.Int(DefaultServiceTimeout),
							WriteTimeout:   kong.Int(DefaultServiceTimeout),
							Retries:        kong.Int(DefaultRetries),
						},
						Namespace: ingress.Namespace,
						Backend: kongstate.ServiceBackend{
							Name: rulePath.Backend.Service.Name,
							Port: port,
						},
					}
				}
				service.Routes = append(service.Routes, r)
				result.ServiceNameToServices[serviceName] = service
			}
		}
	}

	sort.SliceStable(allDefaultBackends, func(i, j int) bool {
		return allDefaultBackends[i].CreationTimestamp.Before(&allDefaultBackends[j].CreationTimestamp)
	})

	// Process the default backend
	if len(allDefaultBackends) > 0 {
		ingress := allDefaultBackends[0]
		defaultBackend := allDefaultBackends[0].Spec.DefaultBackend
		port := PortDefFromServiceBackendPort(&defaultBackend.Service.Port)
		serviceName := fmt.Sprintf("%s.%s.%s", allDefaultBackends[0].Namespace, defaultBackend.Service.Name,
			port.CanonicalString())
		service, ok := result.ServiceNameToServices[serviceName]
		if !ok {
			service = kongstate.Service{
				Service: kong.Service{
					Name: kong.String(serviceName),
					Host: kong.String(fmt.Sprintf("%s.%s.%d.svc", defaultBackend.Service.Name, ingress.Namespace,
						defaultBackend.Service.Port.Number)),
					Port:           kong.Int(DefaultHTTPPort),
					Protocol:       kong.String("http"),
					ConnectTimeout: kong.Int(DefaultServiceTimeout),
					ReadTimeout:    kong.Int(DefaultServiceTimeout),
					WriteTimeout:   kong.Int(DefaultServiceTimeout),
					Retries:        kong.Int(DefaultRetries),
				},
				Namespace: ingress.Namespace,
				Backend: kongstate.ServiceBackend{
					Name: defaultBackend.Service.Name,
					Port: PortDefFromServiceBackendPort(&defaultBackend.Service.Port),
				},
			}
		}
		r := kongstate.Route{
			Ingress: util.FromK8sObject(&ingress),
			Route: kong.Route{
				Name:              kong.String(ingress.Namespace + "." + ingress.Name),
				Paths:             kong.StringSlice("/"),
				StripPath:         kong.Bool(false),
				PreserveHost:      kong.Bool(true),
				Protocols:         kong.StringSlice("http", "https"),
				RegexPriority:     kong.Int(0),
				RequestBuffering:  kong.Bool(true),
				ResponseBuffering: kong.Bool(true),
			},
		}
		service.Routes = append(service.Routes, r)
		result.ServiceNameToServices[serviceName] = service
	}

	return result
}

func fromTCPIngressV1beta1(log logrus.FieldLogger, tcpIngressList []*configurationv1beta1.TCPIngress, hashedRouteNames bool) ingressRules {
	result := newIngressRules()

	sort.SliceStable(tcpIngressList, func(i, j int) bool {
		return tcpIngressList[i].CreationTimestamp.Before(
			&tcpIngressList[j].CreationTimestamp)
	})

	for _, ingress := range tcpIngressList {
		ingressSpec := ingress.Spec

		log = log.WithFields(logrus.Fields{
			"tcpingress_namespace": ingress.Namespace,
			"tcpingress_name":      ingress.Name,
		})

		result.SecretNameToSNIs.addFromIngressV1beta1TLS(tcpIngressToNetworkingTLS(ingressSpec.TLS), ingress.Namespace)

		// a prefix that will be used for all kong.Routes derived from the ingress rules.
		routePrefix := fmt.Sprintf("%s.%s.%s.", TCPIngressV1Beta1RoutePrefix, ingress.Namespace, ingress.Name)

		usedRouteNames := make(map[string]struct{})
		for i, rule := range ingressSpec.Rules {
			if !util.IsValidPort(rule.Port) {
				log.Errorf("invalid TCPIngress: invalid port: %v", rule.Port)
				continue
			}

			// by default if hashedRouteNames is not configured we'll use the legacy
			// convention for naming the route numerically in a sequence according to
			// the order it was configured within the Ingress spec. The downside of
			// this naming convention is that for use cases where the spec updates
			// often this has the potential to cause connection thrash as routes may
			// need to be replaced entirely to maintain the sequence.
			routeName := fmt.Sprintf("%s.%s.%d", ingress.Namespace, ingress.Name, i)

			// if hashed route names are requested, we use a unique hash
			// for the name instead of a numbered sequence.
			if hashedRouteNames {
				// determine the unique name for the route to be created,
				// unique names help to avoid configuration thrash from
				// route names changing due to configuration ordering.
				var err error
				routeName, err = getUniqIDForRouteConfig(rule)
				if err != nil {
					log.Errorf("rule skipped: could not determine unique name: %w", err)
					continue
				}
				routeName = routePrefix + routeName

				// there's technically an infinitesmal chance of a collision,
				// if that happens add the rule order number to the name to
				// ensure uniqueness otherwise this would create a deadlock
				// on data-plane configurations.
				if _, exists := usedRouteNames[routeName]; exists {
					routeName = fmt.Sprintf("%s-%d", routeName, i)
				}
				usedRouteNames[routeName] = struct{}{}
			}

			r := kongstate.Route{
				Ingress: util.FromK8sObject(ingress),
				Route: kong.Route{
					Name:      kong.String(routeName),
					Protocols: kong.StringSlice("tcp", "tls"),
					Destinations: []*kong.CIDRPort{
						{
							Port: kong.Int(rule.Port),
						},
					},
				},
			}
			host := rule.Host
			if host != "" {
				r.SNIs = kong.StringSlice(host)
			}
			if rule.Backend.ServiceName == "" {
				log.Errorf("invalid TCPIngress: empty serviceName")
				continue
			}
			if !util.IsValidPort(rule.Backend.ServicePort) {
				log.Errorf("invalid TCPIngress: invalid servicePort: %v", rule.Backend.ServicePort)
				continue
			}

			serviceName := fmt.Sprintf("%s.%s.%d", ingress.Namespace, rule.Backend.ServiceName, rule.Backend.ServicePort)
			service, ok := result.ServiceNameToServices[serviceName]
			if !ok {
				service = kongstate.Service{
					Service: kong.Service{
						Name: kong.String(serviceName),
						Host: kong.String(fmt.Sprintf("%s.%s.%d.svc", rule.Backend.ServiceName, ingress.Namespace,
							rule.Backend.ServicePort)),
						Port:           kong.Int(DefaultHTTPPort),
						Protocol:       kong.String("tcp"),
						ConnectTimeout: kong.Int(DefaultServiceTimeout),
						ReadTimeout:    kong.Int(DefaultServiceTimeout),
						WriteTimeout:   kong.Int(DefaultServiceTimeout),
						Retries:        kong.Int(DefaultRetries),
					},
					Namespace: ingress.Namespace,
					Backend: kongstate.ServiceBackend{
						Name: rule.Backend.ServiceName,
						Port: kongstate.PortDef{Mode: kongstate.PortModeByNumber, Number: int32(rule.Backend.ServicePort)},
					},
				}
			}
			service.Routes = append(service.Routes, r)
			result.ServiceNameToServices[serviceName] = service
		}
	}

	return result
}

func fromUDPIngressV1beta1(log logrus.FieldLogger, ingressList []*configurationv1beta1.UDPIngress, hashedRouteNames bool) ingressRules {
	result := newIngressRules()

	sort.SliceStable(ingressList, func(i, j int) bool {
		return ingressList[i].CreationTimestamp.Before(&ingressList[j].CreationTimestamp)
	})

	for _, ingress := range ingressList {
		ingressSpec := ingress.Spec

		log = log.WithFields(logrus.Fields{
			"udpingress_namespace": ingress.Namespace,
			"udpingress_name":      ingress.Name,
		})

		// a prefix that will be used for all kong.Routes derived from the ingress rules.
		routePrefix := fmt.Sprintf("%s.%s.%s.", UDPIngressV1Beta1RoutePrefix, ingress.Namespace, ingress.Name)

		usedRouteNames := make(map[string]struct{})
		for i, rule := range ingressSpec.Rules {
			// validate the ports and servicenames for the rule
			if !util.IsValidPort(rule.Port) {
				log.Errorf("invalid UDPIngress: invalid port: %d", rule.Port)
				continue
			}
			if rule.Backend.ServiceName == "" {
				log.Errorf("invalid UDPIngress: empty serviceName")
				continue
			}
			if !util.IsValidPort(rule.Backend.ServicePort) {
				log.Errorf("invalid UDPIngress: invalid servicePort: %d", rule.Backend.ServicePort)
				continue
			}

			// by default if hashedRouteNames is not configured we'll use the legacy
			// convention for naming the route numerically in a sequence according to
			// the order it was configured within the Ingress spec. The downside of
			// this naming convention is that for use cases where the spec updates
			// often this has the potential to cause connection thrash as routes may
			// need to be replaced entirely to maintain the sequence.
			routeName := fmt.Sprintf("%s.%s.%d.udp", ingress.Namespace, ingress.Name, i)

			// if hashed route names are requested, we use a unique hash
			// for the name instead of a numbered sequence.
			if hashedRouteNames {
				// determine the unique name for the route to be created,
				// unique names help to avoid configuration thrash from
				// route names changing due to configuration ordering.
				var err error
				routeName, err = getUniqIDForRouteConfig(rule)
				if err != nil {
					log.Errorf("rule skipped: could not determine unique name: %w", err)
					continue
				}
				routeName = routePrefix + routeName

				// there's technically an infinitesmal chance of a collision,
				// if that happens add the rule order number to the name to
				// ensure uniqueness otherwise this would create a deadlock
				// on data-plane configurations.
				if _, exists := usedRouteNames[routeName]; exists {
					routeName = fmt.Sprintf("%s-%d", routeName, i)
				}
				usedRouteNames[routeName] = struct{}{}
			}

			// generate the kong Route based on the listen port
			route := kongstate.Route{
				Ingress: util.FromK8sObject(ingress),
				Route: kong.Route{
					Name:         kong.String(routeName),
					Protocols:    kong.StringSlice("udp"),
					Destinations: []*kong.CIDRPort{{Port: kong.Int(rule.Port)}},
				},
			}

			// generate the kong Service backend for the UDPIngress rules
			host := fmt.Sprintf("%s.%s.%d.svc", rule.Backend.ServiceName, ingress.Namespace, rule.Backend.ServicePort)
			serviceName := fmt.Sprintf("%s.%s.%d.udp", ingress.Namespace, rule.Backend.ServiceName, rule.Backend.ServicePort)
			service, ok := result.ServiceNameToServices[serviceName]
			if !ok {
				service = kongstate.Service{
					Namespace: ingress.Namespace,
					Service: kong.Service{
						Name:     kong.String(serviceName),
						Protocol: kong.String("udp"),
						Host:     kong.String(host),
						Port:     kong.Int(rule.Backend.ServicePort),
					},
					Backend: kongstate.ServiceBackend{
						Name: rule.Backend.ServiceName,
						Port: kongstate.PortDef{Mode: kongstate.PortModeByNumber, Number: int32(rule.Backend.ServicePort)},
					},
				}
			}
			service.Routes = append(service.Routes, route)
			result.ServiceNameToServices[serviceName] = service
		}
	}

	return result
}

func fromKnativeIngress(log logrus.FieldLogger, ingressList []*knative.Ingress, hashedRouteNames bool) ingressRules {

	sort.SliceStable(ingressList, func(i, j int) bool {
		return ingressList[i].CreationTimestamp.Before(
			&ingressList[j].CreationTimestamp)
	})

	services := map[string]kongstate.Service{}
	secretToSNIs := newSecretNameToSNIs()

	for _, ingress := range ingressList {
		log = log.WithFields(logrus.Fields{
			"knativeingress_namespace": ingress.Namespace,
			"knativeingress_name":      ingress.Name,
		})

		ingressSpec := ingress.Spec

		secretToSNIs.addFromIngressV1beta1TLS(knativeIngressToNetworkingTLS(ingress.Spec.TLS), ingress.Namespace)

		// a prefix that will be used for all kong.Routes derived from the ingress rules.
		routePrefix := fmt.Sprintf("%s.%s.%s.", KnativeIngressV1Alpha1RoutePrefix, ingress.Namespace, ingress.Name)

		usedRouteNames := make(map[string]struct{})
		for i, rule := range ingressSpec.Rules {
			hosts := rule.Hosts
			if rule.HTTP == nil {
				continue
			}
			for j, rulePath := range rule.HTTP.Paths {
				path := rulePath.Path

				if path == "" {
					path = "/"
				}

				// by default if hashedRouteNames is not configured we'll use the legacy
				// convention for naming the route numerically in a sequence according to
				// the order it was configured within the Ingress spec. The downside of
				// this naming convention is that for use cases where the spec updates
				// often this has the potential to cause connection thrash as routes may
				// need to be replaced entirely to maintain the sequence.
				routeName := fmt.Sprintf("%s.%s.%d%d", ingress.Namespace, ingress.Name, i, j)

				// if hashed route names are requested, we use a unique hash
				// for the name instead of a numbered sequence.
				if hashedRouteNames {
					// in order to determine a unique name for the route to
					// be created we need the unique base ingress Rule with
					// the specific HTTP path that we're making the rule for,
					// as this is uniquely identifying information.
					uniqueRule := rule.DeepCopy()
					uniqueRule.HTTP.Paths = []knative.HTTPIngressPath{rulePath}

					// determine the unique name for the route to be created,
					// unique names help to avoid configuration thrash from
					// route names changing due to configuration ordering.
					var err error
					routeName, err = getUniqIDForRouteConfig(uniqueRule)
					if err != nil {
						log.Errorf("rule skipped: could not determine unique name: %w", err)
						continue
					}
					routeName = routePrefix + routeName

					// there's technically an infinitesmal chance of a collision,
					// if that happens add the rule order number to the name to
					// ensure uniqueness otherwise this would create a deadlock
					// on data-plane configurations.
					if _, exists := usedRouteNames[routeName]; exists {
						routeName = fmt.Sprintf("%s-%d-%d", routeName, i, j)
					}
					usedRouteNames[routeName] = struct{}{}
				}

				r := kongstate.Route{
					Ingress: util.FromK8sObject(ingress),
					Route: kong.Route{
						Name:              kong.String(routeName),
						Paths:             kong.StringSlice(path),
						StripPath:         kong.Bool(false),
						PreserveHost:      kong.Bool(true),
						Protocols:         kong.StringSlice("http", "https"),
						RegexPriority:     kong.Int(0),
						RequestBuffering:  kong.Bool(true),
						ResponseBuffering: kong.Bool(true),
					},
				}
				r.Hosts = kong.StringSlice(hosts...)

				knativeBackend := knativeSelectSplit(rulePath.Splits)
				serviceName := fmt.Sprintf("%s.%s.%s", knativeBackend.ServiceNamespace, knativeBackend.ServiceName,
					knativeBackend.ServicePort.String())
				serviceHost := fmt.Sprintf("%s.%s.%s.svc", knativeBackend.ServiceName, knativeBackend.ServiceNamespace,
					knativeBackend.ServicePort.String())
				service, ok := services[serviceName]
				if !ok {

					var headers []string
					for key, value := range knativeBackend.AppendHeaders {
						headers = append(headers, key+":"+value)
					}
					for key, value := range rulePath.AppendHeaders {
						headers = append(headers, key+":"+value)
					}

					service = kongstate.Service{
						Service: kong.Service{
							Name:           kong.String(serviceName),
							Host:           kong.String(serviceHost),
							Port:           kong.Int(DefaultHTTPPort),
							Protocol:       kong.String("http"),
							Path:           kong.String("/"),
							ConnectTimeout: kong.Int(DefaultServiceTimeout),
							ReadTimeout:    kong.Int(DefaultServiceTimeout),
							WriteTimeout:   kong.Int(DefaultServiceTimeout),
							Retries:        kong.Int(DefaultRetries),
						},
						Namespace: ingress.Namespace,
						Backend: kongstate.ServiceBackend{
							Name: knativeBackend.ServiceName,
							Port: PortDefFromIntStr(knativeBackend.ServicePort),
						},
					}
					if len(headers) > 0 {
						service.Plugins = append(service.Plugins, kong.Plugin{
							Name: kong.String("request-transformer"),
							Config: kong.Configuration{
								"add": map[string]interface{}{
									"headers": headers,
								},
							},
						})
					}
				}
				service.Routes = append(service.Routes, r)
				services[serviceName] = service
			}
		}
	}

	return ingressRules{
		ServiceNameToServices: services,
		SecretNameToSNIs:      secretToSNIs,
	}
}

func knativeSelectSplit(splits []knative.IngressBackendSplit) knative.IngressBackendSplit {
	if len(splits) == 0 {
		return knative.IngressBackendSplit{}
	}
	res := splits[0]
	maxPercentage := splits[0].Percent
	if len(splits) == 1 {
		return res
	}
	for i := 1; i < len(splits); i++ {
		if splits[i].Percent > maxPercentage {
			res = splits[i]
			maxPercentage = res.Percent
		}
	}
	return res
}

func pathsFromK8s(path string, pathType networkingv1.PathType) ([]*string, error) {
	switch pathType {
	case networkingv1.PathTypePrefix:
		base := strings.Trim(path, "/")
		if base == "" {
			return kong.StringSlice("/"), nil
		}
		return kong.StringSlice(
			"/"+base+"$",
			"/"+base+"/",
		), nil
	case networkingv1.PathTypeExact:
		relative := strings.TrimLeft(path, "/")
		return kong.StringSlice("/" + relative + "$"), nil
	case networkingv1.PathTypeImplementationSpecific:
		if path == "" {
			return kong.StringSlice("/"), nil
		}
		return kong.StringSlice(path), nil
	}

	return nil, fmt.Errorf("unknown pathType %v", pathType)
}

var priorityForPath = map[networkingv1.PathType]int{
	networkingv1.PathTypeExact:                  300,
	networkingv1.PathTypePrefix:                 200,
	networkingv1.PathTypeImplementationSpecific: 100,
}

func PortDefFromServiceBackendPort(sbp *networkingv1.ServiceBackendPort) kongstate.PortDef {
	switch {
	case sbp.Name != "":
		return kongstate.PortDef{Mode: kongstate.PortModeByName, Name: sbp.Name}
	case sbp.Number != 0:
		return kongstate.PortDef{Mode: kongstate.PortModeByNumber, Number: sbp.Number}
	default:
		return kongstate.PortDef{Mode: kongstate.PortModeImplicit}
	}
}

func PortDefFromIntStr(is intstr.IntOrString) kongstate.PortDef {
	if is.Type == intstr.String {
		return kongstate.PortDef{Mode: kongstate.PortModeByName, Name: is.StrVal}
	}
	return kongstate.PortDef{Mode: kongstate.PortModeByNumber, Number: is.IntVal}
}