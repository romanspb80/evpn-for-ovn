package evpn

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// OVNCtlSource reads OVN state through kubectl exec and ovn-nbctl/sbctl JSON output.
type OVNCtlSource struct {
	Runner      CommandRunner
	KubeContext string
	Namespace   string
	CentralPod  string
	Container   string
}

type ovnTableJSON struct {
	Headings []string `json:"headings"`
	Data     []any    `json:"data"`
}

func decodeOVNJSON(raw string) (*ovnTableJSON, error) {
	var t ovnTableJSON
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func rowList(raw any) []any {
	row, ok := raw.([]any)
	if !ok {
		return nil
	}
	return row
}

func ovsSetToStrings(v any) []string {
	switch val := v.(type) {
	case string:
		if strings.TrimSpace(val) == "" {
			return nil
		}
		return []string{strings.TrimSpace(val)}
	case []any:
		if len(val) == 2 {
			tag, _ := val[0].(string)
			switch tag {
			case "set":
				items, _ := val[1].([]any)
				out := make([]string, 0, len(items))
				for _, item := range items {
					if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
						out = append(out, strings.TrimSpace(s))
					}
				}
				return out
			case "uuid":
				if s, ok := val[1].(string); ok && strings.TrimSpace(s) != "" {
					return []string{strings.TrimSpace(s)}
				}
			}
		}
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

func ovsMapToStringMap(v any) map[string]string {
	res := map[string]string{}
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return res
	}
	tag, _ := arr[0].(string)
	if tag != "map" {
		return res
	}
	items, _ := arr[1].([]any)
	for _, item := range items {
		kv, ok := item.([]any)
		if !ok || len(kv) != 2 {
			continue
		}
		k, _ := kv[0].(string)
		vv, _ := kv[1].(string)
		k = strings.TrimSpace(k)
		vv = strings.TrimSpace(vv)
		if k == "" {
			continue
		}
		res[k] = vv
	}
	return res
}

func (s *OVNCtlSource) runOVN(ctx context.Context, tool string, extraArgs ...string) (string, error) {
	runner := s.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	container := strings.TrimSpace(s.Container)
	if container == "" {
		container = "ovn-ovs"
	}
	args := []string{"--context", s.KubeContext, "-n", s.Namespace, "exec", s.CentralPod, "-c", container, "--", tool}
	args = append(args, extraArgs...)
	return runner.Run(ctx, "kubectl", args...)
}

func (s *OVNCtlSource) ListLogicalSwitchPorts(ctx context.Context) ([]OVNLogicalSwitchPort, error) {
	raw, err := s.runOVN(ctx, "ovn-nbctl", "--format=json", "--columns=name,addresses,external_ids", "list", "Logical_Switch_Port")
	if err != nil {
		return nil, err
	}
	table, err := decodeOVNJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode nb json: %w", err)
	}

	colIndex := map[string]int{}
	for i, h := range table.Headings {
		colIndex[h] = i
	}
	out := []OVNLogicalSwitchPort{}
	for _, rawRow := range table.Data {
		row := rowList(rawRow)
		if row == nil {
			continue
		}
		nameIdx, okName := colIndex["name"]
		addrIdx, okAddr := colIndex["addresses"]
		idsIdx, okIDs := colIndex["external_ids"]
		if !okName || !okAddr || !okIDs || len(row) <= idsIdx {
			continue
		}
		name, _ := row[nameIdx].(string)
		out = append(out, OVNLogicalSwitchPort{
			Name:        strings.TrimSpace(name),
			Addresses:   ovsSetToStrings(row[addrIdx]),
			ExternalIDs: ovsMapToStringMap(row[idsIdx]),
		})
	}
	return out, nil
}

func (s *OVNCtlSource) ListPortBindings(ctx context.Context) ([]OVNPortBinding, error) {
	raw, err := s.runOVN(ctx, "ovn-sbctl", "--format=json", "--columns=logical_port,chassis", "list", "Port_Binding")
	if err != nil {
		return nil, err
	}
	table, err := decodeOVNJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode sb json: %w", err)
	}

	colIndex := map[string]int{}
	for i, h := range table.Headings {
		colIndex[h] = i
	}
	out := []OVNPortBinding{}
	for _, rawRow := range table.Data {
		row := rowList(rawRow)
		if row == nil {
			continue
		}
		lpIdx, okLP := colIndex["logical_port"]
		chIdx, okCh := colIndex["chassis"]
		if !okLP || !okCh || len(row) <= chIdx {
			continue
		}
		lp, _ := row[lpIdx].(string)
		chassisValues := ovsSetToStrings(row[chIdx])
		chassis := ""
		if len(chassisValues) > 0 {
			chassis = chassisValues[0]
		}
		out = append(out, OVNPortBinding{
			LogicalPort: strings.TrimSpace(lp),
			Chassis:     strings.TrimSpace(chassis),
		})
	}
	return out, nil
}
