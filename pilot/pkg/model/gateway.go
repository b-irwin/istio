// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/gateway"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/pkg/monitoring"
)

// ServerPort defines port for the gateway server.
type ServerPort struct {
	// A valid non-negative integer port number.
	Number uint32
	// The protocol exposed on the port.
	Protocol string
	// The bind server specified on this port.
	Bind string
}

// MergedServers describes set of servers defined in all gateways per port.
type MergedServers struct {
	Servers   []*networking.Server
	RouteName string // RouteName for http servers. For HTTPS, TLSServerInfo will hold the route name.
}

// TLSServerInfo contains additional information for TLS Servers.
type TLSServerInfo struct {
	RouteName string
	SNIHosts  []string
}

// MergedGateway describes a set of gateways for a workload merged into a single logical gateway.
type MergedGateway struct {
	// MergedServers maps from physical port to virtual servers.
	MergedServers map[ServerPort]*MergedServers

	// GatewayNameForServer maps from server to the owning gateway name.
	// Used for select the set of virtual services that apply to a port.
	GatewayNameForServer map[*networking.Server]string

	// ServersByRouteName maps from port names to virtual hosts
	// Used for RDS. No two port names share same port except for HTTPS
	// The typical length of the value is always 1, except for HTTP (not HTTPS),
	ServersByRouteName map[string][]*networking.Server

	// TLSServerInfo maps from server to a corresponding TLS information like TLS Routename and SNIHosts.
	TLSServerInfo map[*networking.Server]*TLSServerInfo

	// ContainsAutoPassthroughGateways determines if there are any type AUTO_PASSTHROUGH Gateways, requiring additional
	// clusters to be sent to the workload
	ContainsAutoPassthroughGateways bool

	// PortMap defines a mapping of targetPorts to the set of Service ports that reference them
	PortMap GatewayPortMap
}

var (
	typeTag = monitoring.MustCreateLabel("type")
	nameTag = monitoring.MustCreateLabel("name")

	totalRejectedConfigs = monitoring.NewSum(
		"pilot_total_rejected_configs",
		"Total number of configs that Pilot had to reject or ignore.",
		monitoring.WithLabels(typeTag, nameTag),
	)
)

func init() {
	monitoring.MustRegister(totalRejectedConfigs)
}

func RecordRejectedConfig(gatewayName string) {
	totalRejectedConfigs.With(typeTag.Value("gateway"), nameTag.Value(gatewayName)).Increment()
}

// DisableGatewayPortTranslationLabel is a label on Service that declares that, for that particular
// service, we should not translate Gateway ports to target ports. For example, if I have a Service
// on port 80 with target port 8080, with the label. Gateways on port 80 would *not* match. Instead,
// only Gateways on port 8080 would be used. This prevents ambiguities when there are multiple
// Services on port 80 referring to different target ports. Long term, this will be replaced by
// Gateways directly referencing a Service, rather than label selectors. Warning: this label is
// intended solely for as a workaround for Knative's Istio integration, and not intended for any
// other usage. It can, and will, be removed immediately after the new direct reference is ready for
// use.
const DisableGatewayPortTranslationLabel = "experimental.istio.io/disable-gateway-port-translation"

// MergeGateways combines multiple gateways targeting the same workload into a single logical Gateway.
// Note that today any Servers in the combined gateways listening on the same port must have the same protocol.
// If servers with different protocols attempt to listen on the same port, one of the protocols will be chosen at random.
func MergeGateways(gateways []gatewayWithInstances) *MergedGateway {
	gatewayPorts := make(map[uint32]bool)
	mergedServers := make(map[ServerPort]*MergedServers)
	plainTextServers := make(map[uint32]ServerPort)
	serversByRouteName := make(map[string][]*networking.Server)
	tlsServerInfo := make(map[*networking.Server]*TLSServerInfo)
	gatewayNameForServer := make(map[*networking.Server]string)
	tlsHostsByPort := map[uint32]sets.Set{} // port -> host set
	autoPassthrough := false

	log.Debugf("MergeGateways: merging %d gateways", len(gateways))
	for _, gwAndInstance := range gateways {
		gatewayConfig := gwAndInstance.gateway
		gatewayName := gatewayConfig.Namespace + "/" + gatewayConfig.Name // Format: %s/%s
		gatewayCfg := gatewayConfig.Spec.(*networking.Gateway)
		log.Debugf("MergeGateways: merging gateway %q :\n%v", gatewayName, gatewayCfg)
		snames := sets.Set{}
		for _, s := range gatewayCfg.Servers {
			if len(s.Name) > 0 {
				if snames.Contains(s.Name) {
					log.Warnf("Server name %s is not unique in gateway %s and may create possible issues like stat prefix collision ",
						s.Name, gatewayName)
				} else {
					snames.Insert(s.Name)
				}
			}
			if s.Port == nil {
				// Should be rejected in validation, this is an extra check
				log.Debugf("invalid server without port: %q", gatewayName)
				RecordRejectedConfig(gatewayName)
				continue
			}
			sanitizeServerHostNamespace(s, gatewayConfig.Namespace)
			gatewayNameForServer[s] = gatewayName
			log.Debugf("MergeGateways: gateway %q processing server %s :%v", gatewayName, s.Name, s.Hosts)

			for _, resolvedPort := range resolvePorts(s.Port.Number, gwAndInstance.instances, gwAndInstance.legacyGatewaySelector) {
				routeName := gatewayRDSRouteName(s, resolvedPort, gatewayConfig)
				if s.Tls != nil {
					// Envoy will reject config that has multiple filter chain matches with the same matching rules.
					// To avoid this, we need to make sure we don't have duplicated hosts, which will become
					// SNI filter chain matches.
					if tlsHostsByPort[resolvedPort] == nil {
						tlsHostsByPort[resolvedPort] = sets.NewSet()
					}
					if duplicateHosts := CheckDuplicates(s.Hosts, tlsHostsByPort[resolvedPort]); len(duplicateHosts) != 0 {
						log.Debugf("skipping server on gateway %s, duplicate host names: %v", gatewayName, duplicateHosts)
						RecordRejectedConfig(gatewayName)
						continue
					}
					tlsServerInfo[s] = &TLSServerInfo{SNIHosts: GetSNIHostsForServer(s), RouteName: routeName}
					if s.Tls.Mode == networking.ServerTLSSettings_AUTO_PASSTHROUGH {
						autoPassthrough = true
					}
				}
				serverPort := ServerPort{resolvedPort, s.Port.Protocol, s.Bind}
				serverProtocol := protocol.Parse(serverPort.Protocol)
				if gatewayPorts[resolvedPort] {
					// We have two servers on the same port. Should we merge?
					// 1. Yes if both servers are plain text and HTTP
					// 2. Yes if both servers are using TLS
					//    if using HTTPS ensure that port name is distinct so that we can setup separate RDS
					//    for each server (as each server ends up as a separate http connection manager due to filter chain match)
					// 3. No for everything else.
					if current, exists := plainTextServers[resolvedPort]; exists {
						if !canMergeProtocols(serverProtocol, protocol.Parse(current.Protocol)) {
							log.Infof("skipping server on gateway %s port %s.%d.%s: conflict with existing server %d.%s",
								gatewayConfig.Name, s.Port.Name, resolvedPort, s.Port.Protocol, serverPort.Number, serverPort.Protocol)
							RecordRejectedConfig(gatewayName)
							continue
						}
						if routeName == "" {
							log.Debugf("skipping server on gateway %s port %s.%d.%s: could not build RDS name from server",
								gatewayConfig.Name, s.Port.Name, resolvedPort, s.Port.Protocol)
							RecordRejectedConfig(gatewayName)
							continue
						}
						if current.Bind != serverPort.Bind {
							// Merge it to servers with the same port and bind.
							if mergedServers[serverPort] == nil {
								mergedServers[serverPort] = &MergedServers{Servers: []*networking.Server{}}
							}
							ms := mergedServers[serverPort]
							ms.RouteName = routeName
							ms.Servers = append(ms.Servers, s)
						} else {
							// Merge this to current known port with same bind.
							ms := mergedServers[current]
							ms.Servers = append(ms.Servers, s)
						}
						serversByRouteName[routeName] = append(serversByRouteName[routeName], s)
					} else {
						// We have duplicate port. Its not in plaintext servers. So, this has to be a TLS server.
						// Check if this is also a HTTP server and if so, ensure uniqueness of port name.
						if gateway.IsHTTPServer(s) {
							if routeName == "" {
								log.Debugf("skipping server on gateway %s port %s.%d.%s: could not build RDS name from server",
									gatewayConfig.Name, s.Port.Name, resolvedPort, s.Port.Protocol)
								RecordRejectedConfig(gatewayName)
								continue
							}

							// Both servers are HTTPS servers. Make sure the port names are different so that RDS can pick out individual servers.
							// We cannot have two servers with same port name because we need the port name to distinguish one HTTPS server from another.
							// We cannot merge two HTTPS servers even if their TLS settings have same path to the keys, because we don't know if the contents
							// of the keys are same. So we treat them as effectively different TLS settings.
							// This check is largely redundant now since we create rds names for https using gateway name, namespace
							// and validation ensures that all port names within a single gateway config are unique.
							if _, exists := serversByRouteName[routeName]; exists {
								log.Infof("skipping server on gateway %s port %s.%d.%s: non unique port name for HTTPS port",
									gatewayConfig.Name, s.Port.Name, resolvedPort, s.Port.Protocol)
								RecordRejectedConfig(gatewayName)
								continue
							}
							serversByRouteName[routeName] = []*networking.Server{s}
						}

						// We have another TLS server on the same port. Can differentiate servers using SNI
						if s.Tls == nil {
							log.Warnf("TLS server without TLS options %s %s", gatewayName, s.String())
							continue
						}
						if mergedServers[serverPort] == nil {
							mergedServers[serverPort] = &MergedServers{Servers: []*networking.Server{s}}
						} else {
							mergedServers[serverPort].Servers = append(mergedServers[serverPort].Servers, s)
						}
					}
				} else {
					// This is a new gateway on this port. Create MergedServers for it.
					gatewayPorts[resolvedPort] = true
					if !gateway.IsTLSServer(s) {
						plainTextServers[serverPort.Number] = serverPort
					}
					if gateway.IsHTTPServer(s) {
						serversByRouteName[routeName] = []*networking.Server{s}
					}
					mergedServers[serverPort] = &MergedServers{Servers: []*networking.Server{s}, RouteName: routeName}
				}
				log.Debugf("MergeGateways: gateway %q merged server %v", gatewayName, s.Hosts)
			}
		}
	}

	return &MergedGateway{
		MergedServers:                   mergedServers,
		GatewayNameForServer:            gatewayNameForServer,
		TLSServerInfo:                   tlsServerInfo,
		ServersByRouteName:              serversByRouteName,
		ContainsAutoPassthroughGateways: autoPassthrough,
		PortMap:                         getTargetPortMap(serversByRouteName),
	}
}

// resolvePorts takes a Gateway port, and resolves it to the port that will actually be listened on.
// When legacyGatewaySelector=false, then the gateway is directly referencing a Service. In this
// case, the translation is un-ambiguous - we just find the matching port and return the targetPort
// When legacyGatewaySelector=true things are a bit more complex, as we support referencing a Service
// port and translating to the targetPort in addition to just directly referencing a port. In this
// case, we just make a best effort guess by picking the first match.
func resolvePorts(number uint32, instances []*ServiceInstance, legacyGatewaySelector bool) []uint32 {
	if !features.UseTargetPortForGatewayRoutes {
		return []uint32{number}
	}
	ports := map[uint32]struct{}{}
	for _, w := range instances {
		if _, directPortTranslation := w.Service.Attributes.Labels[DisableGatewayPortTranslationLabel]; directPortTranslation && legacyGatewaySelector {
			// Skip this Service, they opted out of port translation
			// This is only done for legacyGatewaySelector, as the new gateway selection mechanism *only* allows
			// referencing the Service port, and references are un-ambiguous.
			continue
		}
		if w.ServicePort.Port == int(number) && w.Endpoint != nil {
			if legacyGatewaySelector {
				// When we are using legacy gateway label selection, we only resolve to a single port
				// This has pros and cons; we don't allow merging of routes when it would be desirable, but
				// we also avoid accidentally merging routes when we didn't intend to. While neither option is great,
				// picking the first one here preserves backwards compatibility.
				return []uint32{w.Endpoint.EndpointPort}
			}
			ports[w.Endpoint.EndpointPort] = struct{}{}
		}
	}
	ret := make([]uint32, 0, len(ports))
	for p := range ports {
		ret = append(ret, p)
	}
	if len(ret) == 0 && legacyGatewaySelector {
		// When we are using legacy gateway label selection, we should bind to the port as-is if there is
		// no matching ServiceInstance.
		return []uint32{number}
	}
	// For cases where we are directly referencing a Service, we know that they port *must* be in the Service,
	// so we have no fallback. If there was no match, the Gateway is a no-op.
	return ret
}

func canMergeProtocols(current protocol.Instance, p protocol.Instance) bool {
	return (current.IsHTTP() || current == p) && p.IsHTTP()
}

func GetSNIHostsForServer(server *networking.Server) []string {
	if server.Tls == nil {
		return nil
	}
	// sanitize the server hosts as it could contain hosts of form ns/host
	sniHosts := make(map[string]bool)
	for _, h := range server.Hosts {
		if strings.Contains(h, "/") {
			parts := strings.Split(h, "/")
			h = parts[1]
		}
		// do not add hosts, that have already been added
		if !sniHosts[h] {
			sniHosts[h] = true
		}
	}
	sniHostsSlice := make([]string, 0, len(sniHosts))
	for host := range sniHosts {
		sniHostsSlice = append(sniHostsSlice, host)
	}
	sort.Strings(sniHostsSlice)

	return sniHostsSlice
}

// CheckDuplicates returns all of the hosts provided that are already known
// If there were no duplicates, all hosts are added to the known hosts.
func CheckDuplicates(hosts []string, knownHosts sets.Set) []string {
	var duplicates []string
	for _, h := range hosts {
		if knownHosts.Contains(h) {
			duplicates = append(duplicates, h)
		}
	}
	// No duplicates found, so we can mark all of these hosts as known
	if len(duplicates) == 0 {
		for _, h := range hosts {
			knownHosts.Insert(h)
		}
	}
	return duplicates
}

// gatewayRDSRouteName generates the RDS route config name for gateway's servers.
// Unlike sidecars where the RDS route name is the listener port number, gateways have a different
// structure for RDS.
// HTTP servers have route name set to http.<portNumber>.
//   Multiple HTTP servers can exist on the same port and the code will combine all of them into
//   one single RDS payload for http.<portNumber>
// HTTPS servers with TLS termination (i.e. envoy decoding the content, and making outbound http calls to backends)
// will use route name https.<portNumber>.<portName>.<gatewayName>.<namespace>. HTTPS servers using SNI passthrough or
// non-HTTPS servers (e.g., TCP+TLS) with SNI passthrough will be setup as opaque TCP proxies without terminating
// the SSL connection. They would inspect the SNI header and forward to the appropriate upstream as opaque TCP.
//
// Within HTTPS servers terminating TLS, user could setup multiple servers in the gateway. each server could have
// one or more hosts but have different TLS certificates. In this case, we end up having separate filter chain
// for each server, with the filter chain match matching on the server specific TLS certs and SNI headers.
// We have two options here: either have all filter chains use the same RDS route name (e.g. "443") and expose
// all virtual hosts on that port to every filter chain uniformly or expose only the set of virtual hosts
// configured under the server for those certificates. We adopt the latter approach. In other words, each
// filter chain in the multi-filter-chain listener will have a distinct RDS route name
// (https.<portNumber>.<portName>.<gatewayName>.<namespace>) so that when a RDS request comes in, we serve the virtual
// hosts and associated routes for that server.
//
// Note that the common case is one where multiple servers are exposed under a single multi-SAN cert on a single port.
// In this case, we have a single https.<portNumber>.<portName>.<gatewayName>.<namespace> RDS for the HTTPS server.
// While we can use the same RDS route name for two servers (say HTTP and HTTPS) exposing the same set of hosts on
// different ports, the optimization (one RDS instead of two) could quickly become useless the moment the set of
// hosts on the two servers start differing -- necessitating the need for two different RDS routes.
func gatewayRDSRouteName(server *networking.Server, portNumber uint32, cfg config.Config) string {
	p := protocol.Parse(server.Port.Protocol)
	bind := ""
	if server.Bind != "" {
		bind = "." + server.Bind
	}
	if p.IsHTTP() {
		return "http" + "." + strconv.Itoa(int(portNumber)) + bind // Format: http.%d.%s
	}

	if p == protocol.HTTPS && server.Tls != nil && !gateway.IsPassThroughServer(server) {
		return "https" + "." + strconv.Itoa(int(server.Port.Number)) + "." +
			server.Port.Name + "." + cfg.Name + "." + cfg.Namespace + bind // Format: https.%d.%s.%s.%s.%s
	}

	return ""
}

// ParseGatewayRDSRouteName is used by the EnvoyFilter patching logic to match
// a specific route configuration to patch.
func ParseGatewayRDSRouteName(name string) (portNumber int, portName, gatewayName string) {
	parts := strings.Split(name, ".")
	if strings.HasPrefix(name, "http.") {
		// this is a http gateway. Parse port number and return empty string for rest
		if len(parts) >= 2 {
			portNumber, _ = strconv.Atoi(parts[1])
		}
	} else if strings.HasPrefix(name, "https.") {
		if len(parts) >= 5 {
			portNumber, _ = strconv.Atoi(parts[1])
			portName = parts[2]
			// gateway name should be ns/name
			gatewayName = parts[4] + "/" + parts[3]
		}
	}
	return
}

// convert ./host to currentNamespace/Host
// */host to just host
// */* to just *
func sanitizeServerHostNamespace(server *networking.Server, namespace string) {
	for i, h := range server.Hosts {
		if strings.Contains(h, "/") {
			parts := strings.Split(h, "/")
			if parts[0] == "." {
				server.Hosts[i] = fmt.Sprintf("%s/%s", namespace, parts[1])
			} else if parts[0] == "*" {
				if parts[1] == "*" {
					server.Hosts = []string{"*"}
					return
				}
				server.Hosts[i] = parts[1]
			}
		}
	}
}

type GatewayPortMap map[int]map[int]struct{}

func getTargetPortMap(serversByRouteName map[string][]*networking.Server) GatewayPortMap {
	pm := GatewayPortMap{}
	for r, s := range serversByRouteName {
		portNumber, _, _ := ParseGatewayRDSRouteName(r)
		if _, f := pm[portNumber]; !f {
			pm[portNumber] = map[int]struct{}{}
		}
		for _, se := range s {
			if se.Port == nil {
				continue
			}
			pm[portNumber][int(se.Port.Number)] = struct{}{}
		}
	}
	return pm
}
