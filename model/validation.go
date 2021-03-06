// Copyright 2017 Istio Authors
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
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/duration"
	multierror "github.com/hashicorp/go-multierror"

	proxyconfig "istio.io/api/proxy/v1/config"
)

const (
	dns1123LabelMaxLength int    = 63
	dns1123LabelFmt       string = "[a-z0-9]([-a-z0-9]*[a-z0-9])?"

	// TODO: there is a stricter regex for the labels from validation.go in k8s
	qualifiedNameFmt string = "[-A-Za-z0-9_./]*"
)

// Constants for duration fields
const (
	discoveryRefreshDelayMax = time.Minute * 10
	discoveryRefreshDelayMin = time.Second

	connectTimeoutMax = time.Second * 30
	connectTimeoutMin = time.Millisecond

	drainTimeMax          = time.Hour
	parentShutdownTimeMax = time.Hour
)

var (
	dns1123LabelRex = regexp.MustCompile("^" + dns1123LabelFmt + "$")
	tagRegexp       = regexp.MustCompile("^" + qualifiedNameFmt + "$")
)

// IsDNS1123Label tests for a string that conforms to the definition of a label in
// DNS (RFC 1123).
func IsDNS1123Label(value string) bool {
	return len(value) <= dns1123LabelMaxLength && dns1123LabelRex.MatchString(value)
}

// ValidatePort checks that the network port is in range
func ValidatePort(port int) error {
	if 1 <= port && port <= 65535 {
		return nil
	}
	return fmt.Errorf("port number %d must be in the range 1..65535", port)
}

// Validate checks that each name conforms to the spec and has a ProtoMessage
func (descriptor ConfigDescriptor) Validate() error {
	var errs error
	types := make(map[string]bool)
	messages := make(map[string]bool)

	for _, v := range descriptor {
		if v.Key == nil {
			errs = multierror.Append(errs, fmt.Errorf("missing the required key function for type: %q", v.Type))
		}
		if !IsDNS1123Label(v.Type) {
			errs = multierror.Append(errs, fmt.Errorf("invalid type: %q", v.Type))
		}
		if !IsDNS1123Label(v.Plural) {
			errs = multierror.Append(errs, fmt.Errorf("invalid plural: %q", v.Type))
		}
		if proto.MessageType(v.MessageName) == nil {
			errs = multierror.Append(errs, fmt.Errorf("cannot discover proto message type: %q", v.MessageName))
		}
		if _, exists := types[v.Type]; exists {
			errs = multierror.Append(errs, fmt.Errorf("duplicate type: %q", v.Type))
		}
		types[v.Type] = true
		if _, exists := messages[v.MessageName]; exists {
			errs = multierror.Append(errs, fmt.Errorf("duplicate message type: %q", v.MessageName))
		}
		messages[v.MessageName] = true
	}
	return errs
}

// ValidateConfig ensures that the config object is well-defined
func (descriptor ConfigDescriptor) ValidateConfig(typ string, obj interface{}) error {
	if obj == nil {
		return fmt.Errorf("invalid nil configuration object")
	}

	t, ok := descriptor.GetByType(typ)
	if !ok {
		return fmt.Errorf("undeclared type: %q", typ)
	}

	v, ok := obj.(proto.Message)
	if !ok {
		return fmt.Errorf("cannot cast to a proto message")
	}

	if proto.MessageName(v) != t.MessageName {
		return fmt.Errorf("mismatched message type %q and type %q",
			proto.MessageName(v), t.MessageName)
	}

	if err := t.Validate(v); err != nil {
		return err
	}

	return nil
}

// Validate ensures that the service object is well-defined
func (s *Service) Validate() error {
	var errs error
	if len(s.Hostname) == 0 {
		errs = multierror.Append(errs, fmt.Errorf("invalid empty hostname"))
	}
	parts := strings.Split(s.Hostname, ".")
	for _, part := range parts {
		if !IsDNS1123Label(part) {
			errs = multierror.Append(errs, fmt.Errorf("invalid hostname part: %q", part))
		}
	}

	// Require at least one port
	if len(s.Ports) == 0 {
		errs = multierror.Append(errs, fmt.Errorf("service must have at least one declared port"))
	}

	// Port names can be empty if there exists only one port
	for _, port := range s.Ports {
		if port.Name == "" {
			if len(s.Ports) > 1 {
				errs = multierror.Append(errs,
					fmt.Errorf("empty port names are not allowed for services with multiple ports"))
			}
		} else if !IsDNS1123Label(port.Name) {
			errs = multierror.Append(errs, fmt.Errorf("invalid name: %q", port.Name))
		}
		if err := ValidatePort(port.Port); err != nil {
			errs = multierror.Append(errs,
				fmt.Errorf("invalid service port value %d for %q: %v", port.Port, port.Name, err))
		}
	}
	return errs
}

// Validate ensures that the service instance is well-defined
func (instance *ServiceInstance) Validate() error {
	var errs error
	if instance.Service == nil {
		errs = multierror.Append(errs, fmt.Errorf("missing service in the instance"))
	} else if err := instance.Service.Validate(); err != nil {
		errs = multierror.Append(errs, err)
	}

	if err := instance.Tags.Validate(); err != nil {
		errs = multierror.Append(errs, err)
	}

	if err := ValidatePort(instance.Endpoint.Port); err != nil {
		errs = multierror.Append(errs, err)
	}

	port := instance.Endpoint.ServicePort
	if port == nil {
		errs = multierror.Append(errs, fmt.Errorf("missing service port"))
	} else if instance.Service != nil {
		expected, ok := instance.Service.Ports.Get(port.Name)
		if !ok {
			errs = multierror.Append(errs, fmt.Errorf("missing service port %q", port.Name))
		} else {
			if expected.Port != port.Port {
				errs = multierror.Append(errs,
					fmt.Errorf("unexpected service port value %d, expected %d", port.Port, expected.Port))
			}
			if expected.Protocol != port.Protocol {
				errs = multierror.Append(errs,
					fmt.Errorf("unexpected service protocol %s, expected %s", port.Protocol, expected.Protocol))
			}
		}
	}

	return errs
}

// Validate ensures tag is well-formed
func (t Tags) Validate() error {
	var errs error
	for k, v := range t {
		if !tagRegexp.MatchString(k) {
			errs = multierror.Append(errs, fmt.Errorf("invalid tag key: %q", k))
		}
		if !tagRegexp.MatchString(v) {
			errs = multierror.Append(errs, fmt.Errorf("invalid tag value: %q", v))
		}
	}
	return errs
}

// ValidateFQDN checks a fully-qualified domain name
func ValidateFQDN(fqdn string) error {
	if len(fqdn) > 255 {
		return fmt.Errorf("domain name %q too long (max 255)", fqdn)
	}
	if len(fqdn) == 0 {
		return fmt.Errorf("empty domain name not allowed")
	}

	for _, label := range strings.Split(fqdn, ".") {
		if !IsDNS1123Label(label) {
			return fmt.Errorf("domain name %q invalid (label %q invalid)", fqdn, label)
		}
	}

	return nil
}

// ValidateMatchCondition validates a match condition
func ValidateMatchCondition(mc *proxyconfig.MatchCondition) (errs error) {
	if mc.Source != "" {
		if err := ValidateFQDN(mc.Source); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if err := Tags(mc.SourceTags).Validate(); err != nil {
		errs = multierror.Append(errs, err)
	}

	if mc.GetTcp() != nil {
		if err := ValidateL4MatchAttributes(mc.GetTcp()); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if mc.GetUdp() != nil {
		if err := ValidateL4MatchAttributes(mc.GetUdp()); err != nil {
			errs = multierror.Append(errs, err)
		}
		errs = multierror.Append(errs, fmt.Errorf("Istio does not support UDP protocol yet"))
	}

	for name, value := range mc.GetHttpHeaders() {
		if err := ValidateHTTPHeaderName(name); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, fmt.Sprintf("header name %q invalid: ", name)))
		}
		if err := ValidateStringMatch(value); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, fmt.Sprintf("header %q value invalid: ", name)))
		}

		// validate special `uri` header:
		// absolute path must be non-empty (https://www.w3.org/Protocols/rfc2616/rfc2616-sec5.html#sec5.1.2)
		if name == HeaderURI {
			switch m := value.MatchType.(type) {
			case *proxyconfig.StringMatch_Exact:
				if m.Exact == "" {
					errs = multierror.Append(errs, fmt.Errorf("exact header value for %q must be non-empty", HeaderURI))
				}
			case *proxyconfig.StringMatch_Prefix:
				if m.Prefix == "" {
					errs = multierror.Append(errs, fmt.Errorf("prefix header value for %q must be non-empty", HeaderURI))
				}
			case *proxyconfig.StringMatch_Regex:
				if m.Regex == "" {
					errs = multierror.Append(errs, fmt.Errorf("regex header value for %q must be non-empty", HeaderURI))
				}
			}
		}

		// TODO authority special header
	}

	return
}

// ValidateHTTPHeaderName checks that the name is lower-case
func ValidateHTTPHeaderName(name string) error {
	if strings.ToLower(name) != name {
		return fmt.Errorf("must be in lower case")
	}
	return nil
}

// ValidateStringMatch checks that the match types are correct
func ValidateStringMatch(match *proxyconfig.StringMatch) error {
	switch match.MatchType.(type) {
	case *proxyconfig.StringMatch_Exact, *proxyconfig.StringMatch_Prefix, *proxyconfig.StringMatch_Regex:
	default:
		return fmt.Errorf("unrecognized string match %q", match)
	}
	return nil
}

// ValidateL4MatchAttributes validates L4 Match Attributes
func ValidateL4MatchAttributes(ma *proxyconfig.L4MatchAttributes) (errs error) {
	for _, subnet := range ma.SourceSubnet {
		if err := ValidateSubnet(subnet); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	for _, subnet := range ma.DestinationSubnet {
		if err := ValidateSubnet(subnet); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	return
}

// ValidatePercent checks that percent is in range
func ValidatePercent(val int32) error {
	if val < 0 || val > 100 {
		return fmt.Errorf("must be in range 0..100")
	}
	return nil
}

// ValidateFloatPercent checks that percent is in range
func ValidateFloatPercent(val float32) error {
	if val < 0.0 || val > 100.0 {
		return fmt.Errorf("must be in range 0..100")
	}
	return nil
}

// ValidateDestinationWeight validates DestinationWeight
func ValidateDestinationWeight(dw *proxyconfig.DestinationWeight) (errs error) {
	if dw.Destination != "" {
		if err := ValidateFQDN(dw.Destination); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if err := Tags(dw.Tags).Validate(); err != nil {
		errs = multierror.Append(errs, err)
	}

	if err := ValidatePercent(dw.Weight); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "weight invalid: "))
	}

	return
}

// ValidateHTTPTimeout validates HTTP Timeout
func ValidateHTTPTimeout(timeout *proxyconfig.HTTPTimeout) (errs error) {
	if simple := timeout.GetSimpleTimeout(); simple != nil {
		if err := ValidateDuration(simple.Timeout); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "httpTimeout invalid: "))
		}

		// TODO validate override_header_name?
	}

	return
}

// ValidateHTTPRetries validates HTTP Retries
func ValidateHTTPRetries(retry *proxyconfig.HTTPRetry) (errs error) {
	if simple := retry.GetSimpleRetry(); simple != nil {
		if simple.Attempts < 0 {
			errs = multierror.Append(errs, fmt.Errorf("attempts must be in range [0..]"))
		}

		if err := ValidateDuration(simple.PerTryTimeout); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "perTryTimeout invalid: "))
		}
		// We ignore override_header_name
	}

	return
}

// ValidateHTTPFault validates HTTP Fault
func ValidateHTTPFault(fault *proxyconfig.HTTPFaultInjection) (errs error) {
	if fault.GetDelay() != nil {
		if err := ValidateDelay(fault.GetDelay()); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if fault.GetAbort() != nil {
		if err := ValidateAbort(fault.GetAbort()); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	return
}

// ValidateL4Fault validates L4 Fault
func ValidateL4Fault(fault *proxyconfig.L4FaultInjection) (errs error) {
	if fault.GetTerminate() != nil {
		if err := ValidateTerminate(fault.GetTerminate()); err != nil {
			errs = multierror.Append(errs, err)
		}
		errs = multierror.Append(errs, fmt.Errorf("Istio does not support the terminate fault yet"))
	}

	if fault.GetThrottle() != nil {
		if err := ValidateThrottle(fault.GetThrottle()); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	return
}

// ValidateSubnet checks that IPv4 subnet form
func ValidateSubnet(subnet string) error {
	// The current implementation only supports IP v4 addresses
	return ValidateIPv4Subnet(subnet)
}

// ValidateIPv4Subnet checks that a string is in "CIDR notation" or "Dot-decimal notation"
func ValidateIPv4Subnet(subnet string) error {
	// We expect a string in "CIDR notation" or "Dot-decimal notation"
	// E.g., a.b.c.d/xx form or just a.b.c.d
	parts := strings.Split(subnet, "/")
	if len(parts) > 2 {
		return fmt.Errorf("%q is not valid CIDR notation", subnet)
	}

	var errs error

	if len(parts) == 2 {
		if err := ValidateCIDRBlock(parts[1]); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if err := ValidateIPv4Address(parts[0]); err != nil {
		errs = multierror.Append(errs, err)
	}

	return errs
}

// ValidateCIDRBlock validates that a string in "CIDR notation" or "Dot-decimal notation"
func ValidateCIDRBlock(cidr string) error {
	if bits, err := strconv.Atoi(cidr); err != nil || bits <= 0 || bits > 32 {
		return fmt.Errorf("/%v is not a valid CIDR block", cidr)
	}

	return nil
}

// ValidateIPv4Address validates that a string in "CIDR notation" or "Dot-decimal notation"
func ValidateIPv4Address(addr string) error {
	octets := strings.Split(addr, ".")
	if len(octets) != 4 {
		return fmt.Errorf("%q is not a valid IP address", addr)
	}

	for _, octet := range octets {
		if n, err := strconv.Atoi(octet); err != nil || n < 0 || n > 255 {
			return fmt.Errorf("%q is not a valid IP address", addr)
		}
	}

	return nil
}

// ValidateDelay checks that fault injection delay is well-formed
func ValidateDelay(delay *proxyconfig.HTTPFaultInjection_Delay) (errs error) {
	if err := ValidateFloatPercent(delay.Percent); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "percent invalid: "))
	}
	if err := ValidateDuration(delay.GetFixedDelay()); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "fixedDelay invalid:"))
	}

	if delay.GetExponentialDelay() != nil {
		if err := ValidateDuration(delay.GetExponentialDelay()); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "exponentialDelay invalid: "))
		}
		errs = multierror.Append(errs, fmt.Errorf("Istio does not support exponentialDelay yet"))
	}

	return
}

// ValidateAbortHTTPStatus checks that fault injection abort HTTP status is valid
func ValidateAbortHTTPStatus(httpStatus *proxyconfig.HTTPFaultInjection_Abort_HttpStatus) (errs error) {
	if httpStatus.HttpStatus < 0 || httpStatus.HttpStatus > 600 {
		errs = multierror.Append(errs, fmt.Errorf("invalid abort http status %v", httpStatus.HttpStatus))
	}

	return
}

// ValidateAbort checks that fault injection abort is well-formed
func ValidateAbort(abort *proxyconfig.HTTPFaultInjection_Abort) (errs error) {
	if err := ValidateFloatPercent(abort.Percent); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "percent invalid: "))
	}

	switch abort.ErrorType.(type) {
	case *proxyconfig.HTTPFaultInjection_Abort_GrpcStatus:
		// TODO No validation yet for grpc_status / http2_error / http_status
		errs = multierror.Append(errs, fmt.Errorf("Istio does not support gRPC fault injection yet"))
	case *proxyconfig.HTTPFaultInjection_Abort_Http2Error:
		// TODO No validation yet for grpc_status / http2_error / http_status
	case *proxyconfig.HTTPFaultInjection_Abort_HttpStatus:
		if err := ValidateAbortHTTPStatus(abort.ErrorType.(*proxyconfig.HTTPFaultInjection_Abort_HttpStatus)); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	// No validation yet for override_header_name

	return
}

// ValidateTerminate checks that fault injection terminate is well-formed
func ValidateTerminate(terminate *proxyconfig.L4FaultInjection_Terminate) (errs error) {
	if err := ValidateFloatPercent(terminate.Percent); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "terminate percent invalid: "))
	}
	return
}

// ValidateThrottle checks that fault injections throttle is well-formed
func ValidateThrottle(throttle *proxyconfig.L4FaultInjection_Throttle) (errs error) {
	if err := ValidateFloatPercent(throttle.Percent); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "throttle percent invalid: "))
	}

	if throttle.DownstreamLimitBps < 0 {
		errs = multierror.Append(errs, fmt.Errorf("downstreamLimitBps invalid"))
	}

	if throttle.UpstreamLimitBps < 0 {
		errs = multierror.Append(errs, fmt.Errorf("upstreamLimitBps invalid"))
	}

	err := ValidateDuration(throttle.GetThrottleAfterPeriod())
	if err != nil {
		errs = multierror.Append(errs, fmt.Errorf("throttleAfterPeriod invalid"))
	}

	if throttle.GetThrottleAfterBytes() < 0 {
		errs = multierror.Append(errs, fmt.Errorf("throttleAfterBytes invalid"))
	}

	// TODO Check DoubleValue throttle.GetThrottleForSeconds()

	return
}

// ValidateLoadBalancing validates Load Balancing
func ValidateLoadBalancing(lb *proxyconfig.LoadBalancing) (errs error) {
	// Currently the policy is just a name, and we don't validate it
	return
}

// ValidateCircuitBreaker validates Circuit Breaker
func ValidateCircuitBreaker(cb *proxyconfig.CircuitBreaker) (errs error) {
	if simple := cb.GetSimpleCb(); simple != nil {
		if simple.MaxConnections < 0 {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreak maxConnections must be in range [0..]"))
		}
		if simple.HttpMaxPendingRequests < 0 {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreaker maxPendingRequests must be in range [0..]"))
		}
		if simple.HttpMaxRequests < 0 {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreaker maxRequests must be in range [0..]"))
		}

		err := ValidateDuration(simple.SleepWindow)
		if err != nil {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreaker sleepWindow must be in range [0..]"))
		}

		if simple.HttpConsecutiveErrors < 0 {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreaker httpConsecutiveErrors must be in range [0..]"))
		}

		err = ValidateDuration(simple.HttpDetectionInterval)
		if err != nil {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreaker httpDetectionInterval must be in range [0..]"))
		}

		if simple.HttpMaxRequestsPerConnection < 0 {
			errs = multierror.Append(errs,
				fmt.Errorf("circuitBreaker httpMaxRequestsPerConnection must be in range [0..]"))
		}
		if err := ValidatePercent(simple.HttpMaxEjectionPercent); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "circuitBreaker httpMaxEjectionPercent invalid: "))
		}
	}

	return
}

// ValidateWeights checks that destination weights sum to 100
func ValidateWeights(routes []*proxyconfig.DestinationWeight, defaultDestination string) (errs error) {
	// Sum weights
	sum := 0
	for _, destWeight := range routes {
		sum = sum + int(destWeight.Weight)
	}

	// From cfg.proto "If there is only [one] destination in a rule, the weight value is assumed to be 100."
	if len(routes) == 1 && sum == 0 {
		return
	}

	if sum != 100 {
		errs = multierror.Append(errs,
			fmt.Errorf("Route weights total %v (must total 100)", sum))
	}

	return
}

// ValidateRouteRule checks routing rules
func ValidateRouteRule(msg proto.Message) error {
	value, ok := msg.(*proxyconfig.RouteRule)
	if !ok {
		return fmt.Errorf("cannot cast to routing rule")
	}

	var errs error
	if value.Name == "" {
		errs = multierror.Append(errs, fmt.Errorf("route rule must have a name"))
	}
	if !IsDNS1123Label(value.Name) {
		errs = multierror.Append(errs, fmt.Errorf("route rule name must be a host name label"))
	}
	if value.Destination == "" {
		errs = multierror.Append(errs, fmt.Errorf("route rule must have a destination service"))
	}
	if err := ValidateFQDN(value.Destination); err != nil {
		errs = multierror.Append(errs, err)
	}

	// We don't validate precedence because any int32 is legal

	if value.Match != nil {
		if err := ValidateMatchCondition(value.Match); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if value.Rewrite != nil {
		if value.Rewrite.GetUri() == "" && value.Rewrite.GetAuthority() == "" {
			errs = multierror.Append(errs, errors.New("rewrite must specify path, host, or both"))
		}
	}

	if value.Redirect != nil {
		if len(value.Route) > 0 {
			errs = multierror.Append(errs, errors.New("rule cannot contain both route and redirect"))
		}

		if value.HttpFault != nil {
			errs = multierror.Append(errs, errors.New("rule cannot contain both fault and redirect"))
		}

		if value.Redirect.GetAuthority() == "" && value.Redirect.GetUri() == "" {
			errs = multierror.Append(errs, errors.New("redirect must specify path, host, or both"))
		}
	}

	if value.Redirect != nil && value.Rewrite != nil {
		errs = multierror.Append(errs, errors.New("rule cannot contain both rewrite and redirect"))
	}

	if value.Route != nil {
		for _, destWeight := range value.Route {
			if err := ValidateDestinationWeight(destWeight); err != nil {
				errs = multierror.Append(errs, err)
			}
		}
		if err := ValidateWeights(value.Route, value.Destination); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if value.HttpReqTimeout != nil {
		if err := ValidateHTTPTimeout(value.HttpReqTimeout); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if value.HttpReqRetries != nil {
		if err := ValidateHTTPRetries(value.HttpReqRetries); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if value.HttpFault != nil {
		if err := ValidateHTTPFault(value.HttpFault); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if value.L4Fault != nil {
		if err := ValidateL4Fault(value.L4Fault); err != nil {
			errs = multierror.Append(errs, err)
		}
		errs = multierror.Append(errs, fmt.Errorf("L4 faults are not implemented"))
	}

	return errs
}

// ValidateIngressRule checks ingress rules
func ValidateIngressRule(msg proto.Message) error {
	value, ok := msg.(*proxyconfig.IngressRule)
	if !ok {
		return fmt.Errorf("cannot cast to ingress rule")
	}

	var errs error
	if value.Name == "" {
		errs = multierror.Append(errs, fmt.Errorf("ingress rule must have a name"))
	}
	if !IsDNS1123Label(value.Name) {
		errs = multierror.Append(errs, fmt.Errorf("ingress rule name must be a host name label"))
	}
	if value.Destination == "" {
		errs = multierror.Append(errs, fmt.Errorf("ingress rule must have a destination service"))
	}
	if err := ValidateFQDN(value.Destination); err != nil {
		errs = multierror.Append(errs, err)
	}

	// TODO: complete validation for ingress
	return errs
}

// ValidateDestinationPolicy checks proxy policies
func ValidateDestinationPolicy(msg proto.Message) error {
	value, ok := msg.(*proxyconfig.DestinationPolicy)
	if !ok {
		return fmt.Errorf("cannot cast to destination policy")
	}

	var errs error

	if value.Destination == "" {
		errs = multierror.Append(errs,
			fmt.Errorf("destination policy should have a valid service name in its destination field"))
	} else {
		if err := ValidateFQDN(value.Destination); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	for _, policy := range value.Policy {
		if err := Tags(policy.Tags).Validate(); err != nil {
			errs = multierror.Append(errs, err)
		}

		if policy.GetLoadBalancing() != nil {
			if err := ValidateLoadBalancing(policy.GetLoadBalancing()); err != nil {
				errs = multierror.Append(errs, err)
			}
		}

		if policy.GetCircuitBreaker() != nil {
			if err := ValidateCircuitBreaker(policy.GetCircuitBreaker()); err != nil {
				errs = multierror.Append(errs, err)
			}
		}
	}

	return errs
}

// ValidateProxyAddress checks that a network address is well-formed
func ValidateProxyAddress(hostAddr string) error {
	colon := strings.Index(hostAddr, ":")
	if colon < 0 {
		return fmt.Errorf("':' separator not found in %q, host address must be of the form <DNS name>:<port> or <IP>:<port>",
			hostAddr)
	}
	port, err := strconv.Atoi(hostAddr[colon+1:])
	if err != nil {
		return err
	}
	if err = ValidatePort(port); err != nil {
		return err
	}
	host := hostAddr[:colon]
	if err = ValidateFQDN(host); err != nil {
		if err = ValidateIPv4Address(host); err != nil {
			return fmt.Errorf("%q is not a valid hostname or an IPv4 address", host)
		}
	}

	return nil
}

// ValidateDuration checks that a proto duration is well-formed
func ValidateDuration(pd *duration.Duration) error {
	dur, err := ptypes.Duration(pd)
	if err != nil {
		return err
	}
	if dur < (1 * time.Millisecond) {
		return errors.New("duration must be greater than 1ms")
	}
	if dur%time.Millisecond != 0 {
		return errors.New("Istio only supports durations to ms precision")
	}
	return nil
}

// ValidateDurationRange verifies range is in specified duration
func ValidateDurationRange(dur, min, max time.Duration) error {
	if dur > max || dur < min {
		return fmt.Errorf("time %v must be >%v and <%v", dur.String(), min.String(), max.String())
	}

	return nil
}

// ValidateParentAndDrain checks that parent and drain durations are valid
func ValidateParentAndDrain(drainTime, parentShutdown *duration.Duration) (errs error) {
	if err := ValidateDuration(drainTime); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid drain duration:"))
	}
	if err := ValidateDuration(parentShutdown); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid parent shutdown duration:"))
	}
	if errs != nil {
		return
	}

	drainDuration, _ := ptypes.Duration(drainTime)
	parentShutdownDuration, _ := ptypes.Duration(parentShutdown)

	if drainDuration%time.Second != 0 {
		errs = multierror.Append(errs,
			errors.New("Istio drain time only supports durations to seconds precision"))
	}
	if parentShutdownDuration%time.Second != 0 {
		errs = multierror.Append(errs,
			errors.New("Istio parent shutdown time only supports durations to seconds precision"))
	}
	if parentShutdownDuration <= drainDuration {
		errs = multierror.Append(errs,
			fmt.Errorf("Istio parent shutdown time %v must be greater than drain time %v",
				parentShutdownDuration.String(), drainDuration.String()))
	}

	if drainDuration > drainTimeMax {
		errs = multierror.Append(errs,
			fmt.Errorf("Istio drain time %v must be <%v", drainDuration.String(), drainTimeMax.String()))
	}

	if parentShutdownDuration > parentShutdownTimeMax {
		errs = multierror.Append(errs,
			fmt.Errorf("Istio parent shutdown time %v must be <%v",
				parentShutdownDuration.String(), parentShutdownTimeMax.String()))
	}

	return
}

// ValidateRefreshDelay validates the discovery refresh delay time
func ValidateRefreshDelay(refresh *duration.Duration) error {
	if err := ValidateDuration(refresh); err != nil {
		return err
	}

	refreshDuration, _ := ptypes.Duration(refresh)
	err := ValidateDurationRange(refreshDuration, discoveryRefreshDelayMin, discoveryRefreshDelayMax)
	return err
}

// ValidateConnectTimeout validates the envoy conncection timeout
func ValidateConnectTimeout(timeout *duration.Duration) error {
	if err := ValidateDuration(timeout); err != nil {
		return err
	}

	timeoutDuration, _ := ptypes.Duration(timeout)
	err := ValidateDurationRange(timeoutDuration, connectTimeoutMin, connectTimeoutMax)
	return err
}

// ValidateProxyMeshConfig checks that the mesh config is well-formed
func ValidateProxyMeshConfig(mesh *proxyconfig.ProxyMeshConfig) (errs error) {
	if mesh.EgressProxyAddress != "" {
		if err := ValidateProxyAddress(mesh.EgressProxyAddress); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "invalid egress proxy address:"))
		}
	}

	// discovery address is mandatory since mutual TLS relies on CDS.
	// strictly speaking, proxies can operate without RDS/CDS and with hot restarts
	// but that requires additional test validation
	if mesh.DiscoveryAddress == "" {
		errs = multierror.Append(errs, errors.New("discovery address must be set to the proxy discovery service"))
	} else if err := ValidateProxyAddress(mesh.DiscoveryAddress); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid discovery address:"))
	}

	if mesh.MixerAddress != "" {
		if err := ValidateProxyAddress(mesh.MixerAddress); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "invalid Mixer address:"))
		}
	}

	if mesh.StatsdUdpAddress != "" {
		if err := ValidateProxyAddress(mesh.StatsdUdpAddress); err != nil {
			errs = multierror.Append(errs, multierror.Prefix(err, "invalid statsd udp address:"))
		}
	}

	if err := ValidatePort(int(mesh.ProxyListenPort)); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid proxy listen port:"))
	}

	if err := ValidatePort(int(mesh.ProxyAdminPort)); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid proxy admin port:"))
	}

	if mesh.IstioServiceCluster == "" {
		errs = multierror.Append(errs, errors.New("Istio service cluster must be set"))
	}

	if err := ValidateParentAndDrain(mesh.DrainDuration, mesh.ParentShutdownDuration); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid parent and drain time combination"))
	}

	if err := ValidateRefreshDelay(mesh.DiscoveryRefreshDelay); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid refresh delay:"))
	}

	if err := ValidateConnectTimeout(mesh.ConnectTimeout); err != nil {
		errs = multierror.Append(errs, multierror.Prefix(err, "invalid connect timeout:"))
	}

	if mesh.AuthCertsPath == "" {
		errs = multierror.Append(errs, errors.New("invalid auth certificates path"))
	}

	switch mesh.AuthPolicy {
	case proxyconfig.ProxyMeshConfig_NONE, proxyconfig.ProxyMeshConfig_MUTUAL_TLS:
	default:
		errs = multierror.Append(errs, fmt.Errorf("unrecognized auth policy %q", mesh.AuthPolicy))
	}

	return
}
