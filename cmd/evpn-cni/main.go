package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cni100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultTimeoutSeconds = 10
	defaultDialTimeout    = 3 * time.Second
)

var printResult = types.PrintResult

type pluginConfig struct {
	types.NetConf

	NodeAgentSocket   string   `json:"nodeAgentSocket"`
	NetworkID         string   `json:"networkID"`
	LogicalSwitch     string   `json:"logicalSwitch"`
	AttachmentID      string   `json:"attachmentID"`
	IPAddress         string   `json:"ipAddress,omitempty"`
	MACAddress        string   `json:"macAddress,omitempty"`
	OVSInterfaceName  string   `json:"ovsInterfaceName,omitempty"`
	TimeoutSeconds    int      `json:"timeoutSeconds,omitempty"`
	RequestID         string   `json:"requestID,omitempty"`
	EndpointID        string   `json:"endpointID,omitempty"`
	WorkloadUID       string   `json:"workloadUID,omitempty"`
	Incarnation       string   `json:"incarnation,omitempty"`
	SupportedVersions []string `json:"supportedVersions,omitempty"`
}

type cniArgs struct {
	types.CommonArgs
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_UID                types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
	K8S_NODE_NAME              types.UnmarshallableString
	REQUEST_ID                 types.UnmarshallableString
	WORKLOAD_UID               types.UnmarshallableString
	INCARNATION                types.UnmarshallableString
	ENDPOINT_ID                types.UnmarshallableString
}

func main() {
	funcs := skel.CNIFuncs{
		Add:   cmdAdd,
		Del:   cmdDel,
		Check: cmdCheck,
	}
	skel.PluginMainFuncs(funcs, version.PluginSupports("1.0.0"), "evpn-cni")
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, parsedArgs, err := parseConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	req := buildAddRequest(conf, parsedArgs, args)
	client, closeClient, err := newNodeAgentClient(conf.NodeAgentSocket, dialTimeout(conf))
	if err != nil {
		return err
	}
	defer closeClient()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout(conf))
	defer cancel()
	resp, err := client.Add(ctx, req)
	if err != nil {
		return fmt.Errorf("node-agent Add RPC failed: %w", err)
	}
	if resp == nil || !resp.GetSuccess() {
		return fmt.Errorf("node-agent Add unsuccessful: %s", strings.TrimSpace(resp.GetMessage()))
	}
	result, err := toCNIResult(resp.GetResult(), conf.CNIVersion)
	if err != nil {
		return fmt.Errorf("failed to translate Add result: %w", err)
	}
	return printResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, parsedArgs, err := parseConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	req := buildDelRequest(conf, parsedArgs, args)
	client, closeClient, err := newNodeAgentClient(conf.NodeAgentSocket, dialTimeout(conf))
	if err != nil {
		return err
	}
	defer closeClient()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout(conf))
	defer cancel()
	resp, err := client.Del(ctx, req)
	if err != nil {
		return fmt.Errorf("node-agent Del RPC failed: %w", err)
	}
	if resp == nil || !resp.GetSuccess() {
		return fmt.Errorf("node-agent Del unsuccessful: %s", strings.TrimSpace(resp.GetMessage()))
	}

	switch resp.GetOutcome() {
	case nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED,
		nodeagentpb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT,
		nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION:
		return nil
	default:
		return fmt.Errorf("node-agent Del returned unsupported outcome: %s", resp.GetOutcome().String())
	}
}

func cmdCheck(args *skel.CmdArgs) error {
	conf, parsedArgs, err := parseConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	req := buildCheckRequest(conf, parsedArgs, args)
	client, closeClient, err := newNodeAgentClient(conf.NodeAgentSocket, dialTimeout(conf))
	if err != nil {
		return err
	}
	defer closeClient()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout(conf))
	defer cancel()
	resp, err := client.Check(ctx, req)
	if err != nil {
		return fmt.Errorf("node-agent Check RPC failed: %w", err)
	}
	if resp == nil || !resp.GetSuccess() {
		return fmt.Errorf("node-agent Check unsuccessful: %s", strings.TrimSpace(resp.GetMessage()))
	}
	return nil
}

func parseConfig(stdinData []byte, rawArgs string) (*pluginConfig, *cniArgs, error) {
	var conf pluginConfig
	if err := json.Unmarshal(stdinData, &conf); err != nil {
		return nil, nil, fmt.Errorf("parse CNI config: %w", err)
	}
	conf.NodeAgentSocket = strings.TrimSpace(conf.NodeAgentSocket)
	conf.NetworkID = strings.TrimSpace(conf.NetworkID)
	conf.LogicalSwitch = strings.TrimSpace(conf.LogicalSwitch)
	conf.AttachmentID = strings.TrimSpace(conf.AttachmentID)
	conf.IPAddress = strings.TrimSpace(conf.IPAddress)
	conf.MACAddress = strings.TrimSpace(conf.MACAddress)
	conf.OVSInterfaceName = strings.TrimSpace(conf.OVSInterfaceName)
	conf.RequestID = strings.TrimSpace(conf.RequestID)
	conf.EndpointID = strings.TrimSpace(conf.EndpointID)
	conf.WorkloadUID = strings.TrimSpace(conf.WorkloadUID)
	conf.Incarnation = strings.TrimSpace(conf.Incarnation)

	if conf.NodeAgentSocket == "" {
		return nil, nil, errors.New("invalid CNI config: nodeAgentSocket is required")
	}
	if conf.NetworkID == "" {
		return nil, nil, errors.New("invalid CNI config: networkID is required")
	}
	if conf.AttachmentID == "" {
		return nil, nil, errors.New("invalid CNI config: attachmentID is required")
	}

	if conf.TimeoutSeconds <= 0 {
		conf.TimeoutSeconds = defaultTimeoutSeconds
	}
	if conf.CNIVersion == "" {
		return nil, nil, errors.New("invalid CNI config: cniVersion is required")
	}
	if len(conf.SupportedVersions) > 0 {
		supported := false
		for _, v := range conf.SupportedVersions {
			if strings.TrimSpace(v) == conf.CNIVersion {
				supported = true
				break
			}
		}
		if !supported {
			return nil, nil, fmt.Errorf("invalid CNI config: cniVersion %q is not present in supportedVersions", conf.CNIVersion)
		}
	}

	parsed := &cniArgs{}
	if strings.TrimSpace(rawArgs) != "" {
		if err := types.LoadArgs(rawArgs, parsed); err != nil {
			return nil, nil, fmt.Errorf("parse CNI_ARGS: %w", err)
		}
	}
	kubernetes := firstNonEmpty(string(parsed.K8S_POD_NAME), string(parsed.K8S_POD_NAMESPACE), string(parsed.K8S_POD_UID), string(parsed.K8S_POD_INFRA_CONTAINER_ID)) != ""
	if kubernetes && firstNonEmpty(conf.WorkloadUID, string(parsed.WORKLOAD_UID), string(parsed.K8S_POD_UID)) == "" {
		return nil, nil, errors.New("Kubernetes CNI request requires K8S_POD_UID or explicit workloadUID; refusing ambiguous identity")
	}
	return &conf, parsed, nil
}

func buildAddRequest(conf *pluginConfig, parsed *cniArgs, args *skel.CmdArgs) *nodeagentpb.CNIAddRequest {
	return &nodeagentpb.CNIAddRequest{
		Runtime:    buildRuntimeContext(conf, args),
		K8S:        buildK8sIdentity(parsed),
		Identity:   buildEndpointIdentity(conf, parsed, args),
		Attachment: buildAttachment(conf, args),
		Meta:       buildRequestMeta(conf, parsed),
	}
}

func buildDelRequest(conf *pluginConfig, parsed *cniArgs, args *skel.CmdArgs) *nodeagentpb.CNIDelRequest {
	return &nodeagentpb.CNIDelRequest{
		Runtime:    buildRuntimeContext(conf, args),
		K8S:        buildK8sIdentity(parsed),
		Identity:   buildEndpointIdentity(conf, parsed, args),
		Attachment: buildAttachment(conf, args),
		Meta:       buildRequestMeta(conf, parsed),
	}
}

func buildCheckRequest(conf *pluginConfig, parsed *cniArgs, args *skel.CmdArgs) *nodeagentpb.CNICheckRequest {
	return &nodeagentpb.CNICheckRequest{
		Runtime:    buildRuntimeContext(conf, args),
		K8S:        buildK8sIdentity(parsed),
		Identity:   buildEndpointIdentity(conf, parsed, args),
		Attachment: buildAttachment(conf, args),
		Meta:       buildRequestMeta(conf, parsed),
	}
}

func buildRuntimeContext(conf *pluginConfig, args *skel.CmdArgs) *nodeagentpb.CNIRuntimeContext {
	return &nodeagentpb.CNIRuntimeContext{
		CniVersion:  conf.CNIVersion,
		ContainerId: strings.TrimSpace(args.ContainerID),
		NetnsPath:   strings.TrimSpace(args.Netns),
		IfName:      strings.TrimSpace(args.IfName),
	}
}

func buildK8sIdentity(parsed *cniArgs) *nodeagentpb.K8SIdentity {
	if parsed == nil {
		return nil
	}
	return &nodeagentpb.K8SIdentity{
		PodNamespace: string(parsed.K8S_POD_NAMESPACE),
		PodName:      string(parsed.K8S_POD_NAME),
		PodUid:       string(parsed.K8S_POD_UID),
		NodeName:     string(parsed.K8S_NODE_NAME),
	}
}

func buildEndpointIdentity(conf *pluginConfig, parsed *cniArgs, args *skel.CmdArgs) *nodeagentpb.EndpointIdentity {
	workloadUID := firstNonEmpty(conf.WorkloadUID, string(parsed.WORKLOAD_UID), string(parsed.K8S_POD_UID), strings.TrimSpace(args.ContainerID))
	endpointID := firstNonEmpty(conf.EndpointID, string(parsed.ENDPOINT_ID))
	if endpointID == "" {
		endpointID = fmt.Sprintf("%s|%s|%s", conf.NetworkID, conf.AttachmentID, workloadUID)
	}
	incarnation := firstNonEmpty(conf.Incarnation, string(parsed.INCARNATION), strings.TrimSpace(args.ContainerID))

	return &nodeagentpb.EndpointIdentity{
		EndpointId:   endpointID,
		NetworkId:    conf.NetworkID,
		AttachmentId: conf.AttachmentID,
		WorkloadUid:  workloadUID,
		Incarnation:  incarnation,
	}
}

func buildAttachment(conf *pluginConfig, args *skel.CmdArgs) *nodeagentpb.AttachmentRequest {
	return &nodeagentpb.AttachmentRequest{
		LogicalSwitch:    conf.LogicalSwitch,
		OvsInterfaceName: firstNonEmpty(conf.OVSInterfaceName, strings.TrimSpace(args.IfName)),
		StaticIpCidr:     conf.IPAddress,
		StaticMac:        conf.MACAddress,
	}
}

func buildRequestMeta(conf *pluginConfig, parsed *cniArgs) *nodeagentpb.RequestMeta {
	requestID := firstNonEmpty(conf.RequestID, string(parsed.REQUEST_ID))
	if requestID == "" {
		requestID = uuid.NewString()
	}

	return &nodeagentpb.RequestMeta{
		RequestId:      requestID,
		TimeoutSeconds: uint32(conf.TimeoutSeconds),
	}
}

func newNodeAgentClient(socketPath string, timeout time.Duration) (nodeagentpb.NodeAgentCNIClient, func(), error) {
	target := "unix://" + socketPath
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := grpc.DialContext(
		ctx,
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial node-agent socket %q: %w", socketPath, err)
	}

	closeFn := func() {
		if closeErr := conn.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to close node-agent gRPC connection: %v\n", closeErr)
		}
	}
	return nodeagentpb.NewNodeAgentCNIClient(conn), closeFn, nil
}

func toCNIResult(pbResult *nodeagentpb.CNIResult, cniVersion string) (*cni100.Result, error) {
	if pbResult == nil {
		return nil, errors.New("missing CNI result")
	}

	result := &cni100.Result{
		CNIVersion: cniVersion,
	}
	if strings.TrimSpace(result.CNIVersion) == "" {
		result.CNIVersion = strings.TrimSpace(pbResult.GetCniVersion())
	}

	for _, iface := range pbResult.GetInterfaces() {
		result.Interfaces = append(result.Interfaces, &cni100.Interface{
			Name:    strings.TrimSpace(iface.GetName()),
			Mac:     strings.TrimSpace(iface.GetMac()),
			Sandbox: strings.TrimSpace(iface.GetSandbox()),
		})
	}

	for _, ip := range pbResult.GetIps() {
		address, err := parseCIDR(strings.TrimSpace(ip.GetAddress()))
		if err != nil {
			return nil, fmt.Errorf("invalid IP address %q: %w", ip.GetAddress(), err)
		}
		ipCfg := &cni100.IPConfig{Address: *address}
		if ip.GetGateway() != "" {
			gw := net.ParseIP(strings.TrimSpace(ip.GetGateway()))
			if gw == nil {
				return nil, fmt.Errorf("invalid gateway %q", ip.GetGateway())
			}
			ipCfg.Gateway = gw
		}
		if len(result.Interfaces) > 0 {
			idx := int(ip.GetInterface())
			if idx < 0 || idx >= len(result.Interfaces) {
				return nil, fmt.Errorf("ip interface index %d out of range", idx)
			}
			ipCfg.Interface = &idx
		}
		result.IPs = append(result.IPs, ipCfg)
	}

	for _, route := range pbResult.GetRoutes() {
		dst, err := parseCIDR(strings.TrimSpace(route.GetDst()))
		if err != nil {
			return nil, fmt.Errorf("invalid route destination %q: %w", route.GetDst(), err)
		}
		routeCfg := &types.Route{Dst: *dst}
		if route.GetGw() != "" {
			gw := net.ParseIP(strings.TrimSpace(route.GetGw()))
			if gw == nil {
				return nil, fmt.Errorf("invalid route gateway %q", route.GetGw())
			}
			routeCfg.GW = gw
		}
		result.Routes = append(result.Routes, routeCfg)
	}

	if dns := pbResult.GetDns(); dns != nil {
		result.DNS = types.DNS{
			Nameservers: append([]string(nil), dns.GetNameservers()...),
			Search:      append([]string(nil), dns.GetSearches()...),
			Options:     append([]string(nil), dns.GetOptions()...),
			Domain:      strings.TrimSpace(dns.GetDomain()),
		}
	}

	return result, nil
}

func parseCIDR(v string) (*net.IPNet, error) {
	ip, cidr, err := net.ParseCIDR(v)
	if err != nil {
		return nil, err
	}
	cidr.IP = ip
	return cidr, nil
}

func rpcTimeout(conf *pluginConfig) time.Duration {
	return time.Duration(conf.TimeoutSeconds) * time.Second
}

func dialTimeout(conf *pluginConfig) time.Duration {
	timeout := rpcTimeout(conf)
	if timeout <= defaultDialTimeout {
		return timeout
	}
	return defaultDialTimeout
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
