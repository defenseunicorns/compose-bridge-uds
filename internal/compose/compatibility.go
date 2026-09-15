package compose

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

type CompatibilityIssue struct {
	Code        string
	Path        string
	Message     string
	Remediation string
}

type CompatibilityError struct {
	Issues []CompatibilityIssue
}

func (e *CompatibilityError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "compose model contains %d unsupported setting(s)", len(e.Issues))
	for _, issue := range e.Issues {
		fmt.Fprintf(&b, "\n- [%s] %s: %s", issue.Code, issue.Path, issue.Message)
		if issue.Remediation != "" {
			fmt.Fprintf(&b, "; %s", issue.Remediation)
		}
	}
	return b.String()
}

var supportedServiceKeys = map[string]struct{}{
	"build": {}, "cap_add": {}, "cap_drop": {}, "command": {}, "configs": {},
	"container_name": {}, "depends_on": {}, "deploy": {}, "entrypoint": {},
	"environment": {}, "env_file": {}, "expose": {}, "healthcheck": {}, "hostname": {},
	"image": {}, "networks": {}, "ports": {}, "privileged": {}, "profiles": {}, "restart": {},
	"secrets": {}, "security_opt": {}, "stdin_open": {}, "user": {}, "volumes": {},
	"pre_start": {}, "working_dir": {},
}

var unsupportedServiceRemediation = map[string]string{
	"labels":       "replace Docker runtime discovery with explicit UDS networking configuration",
	"network_mode": "use ordinary Compose service networking",
	"platform":     "use an OCI image compatible with the target Kubernetes nodes",
	"runtime":      "use the cluster's standard OCI runtime",
	"stop_signal":  "update the image to shut down correctly on SIGTERM",
	"sysctls":      "remove host kernel tuning from the application package",
}

func validateCompatibility(project types.Project, raw map[string]any, excludedServices map[string]struct{}) error {
	issues := []CompatibilityIssue{}
	includedNetworks := map[string]struct{}{}
	rawServices, _ := asMap(raw["services"])
	serviceNames := make([]string, 0, len(project.Services))
	for name := range project.Services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)

	for _, serviceName := range serviceNames {
		if _, excluded := excludedServices[serviceName]; excluded {
			continue
		}
		service := project.Services[serviceName]
		path := "services." + serviceName
		rawService, _ := asMap(rawServices[serviceName])

		if strings.TrimSpace(service.Image) == "" && service.Build == nil {
			issues = append(issues, CompatibilityIssue{
				Code:        "image-required",
				Path:        path + ".image",
				Message:     "the service has no container image or build definition",
				Remediation: "declare `image:` or `build:`",
			})
		}

		for _, mount := range service.Volumes {
			mountType := strings.ToLower(strings.TrimSpace(mount.Type))
			if mountType == "" && looksLikePath(mount.Source) {
				mountType = types.VolumeTypeBind
			}
			switch mountType {
			case types.VolumeTypeBind:
				continue
			case "", types.VolumeTypeVolume:
			default:
				issues = append(issues, CompatibilityIssue{
					Code:        "volume-type",
					Path:        path + ".volumes",
					Message:     fmt.Sprintf("volume type %q is not supported", mount.Type),
					Remediation: "use a named volume, Compose config, or Compose secret",
				})
			}
		}

		if containerName := strings.TrimSpace(service.ContainerName); containerName != "" {
			fmt.Fprintf(os.Stderr, "warning: service %q container_name %q ignored; Kubernetes resources use the Compose service name\n", serviceName, containerName)
		}
		for networkName, network := range service.Networks {
			includedNetworks[networkName] = struct{}{}
			if network == nil {
				continue
			}
			if len(network.Aliases) > 0 || len(network.DriverOpts) > 0 || network.InterfaceName != "" || network.Ipv4Address != "" || network.Ipv6Address != "" || len(network.LinkLocalIPs) > 0 || network.MacAddress != "" {
				issues = append(issues, CompatibilityIssue{
					Code:        "network-options",
					Path:        path + ".networks." + networkName,
					Message:     "network aliases, addresses, interface names, and driver options are not translated",
					Remediation: "use Kubernetes service names and cluster-assigned addresses",
				})
			}
		}

		for dependencyName := range service.DependsOn {
			if _, excluded := excludedServices[dependencyName]; excluded {
				continue
			}
			dependency, exists := project.Services[dependencyName]
			if !exists || hasDeclaredTCPPort(dependency) {
				continue
			}
			issues = append(issues, CompatibilityIssue{
				Code:        "dependency-port",
				Path:        path + ".depends_on." + dependencyName,
				Message:     fmt.Sprintf("dependency %q has no declared TCP service port for generated wait logic", dependencyName),
				Remediation: fmt.Sprintf("declare a TCP port in ports or expose on service %q, or mark the dependency required: false", dependencyName),
			})
		}

		for i, hook := range service.PreStart {
			if !hook.PerReplica {
				issues = append(issues, CompatibilityIssue{
					Code:        "pre-start-per-replica",
					Path:        fmt.Sprintf("%s.pre_start[%d].per_replica", path, i),
					Message:     "Kubernetes init containers run once per Pod, but this hook requests once-per-service execution",
					Remediation: "set `per_replica: true` or move once-per-service initialization outside the workload",
				})
			}
		}

		for key := range rawService {
			if strings.HasPrefix(key, "x-") {
				continue
			}
			if _, ok := supportedServiceKeys[key]; ok {
				continue
			}
			remediation := unsupportedServiceRemediation[key]
			if remediation == "" {
				remediation = "remove the setting or provide an equivalent through supported Compose and x-uds fields"
			}
			issues = append(issues, CompatibilityIssue{
				Code:        "service-field",
				Path:        path + "." + key,
				Message:     fmt.Sprintf("Compose field %q is not translated", key),
				Remediation: remediation,
			})
		}
	}

	for name, network := range project.Networks {
		if _, used := includedNetworks[name]; !used {
			continue
		}
		unsupportedDriver := network.Driver != "" && network.Driver != "bridge"
		if unsupportedDriver || len(network.DriverOpts) > 0 || network.Internal || network.Attachable || len(network.Ipam.Config) > 0 || network.Ipam.Driver != "" {
			issues = append(issues, CompatibilityIssue{
				Code:        "network-options",
				Path:        "networks." + name,
				Message:     "network driver, IPAM, internal, and attachable settings are not translated",
				Remediation: "use a standard shared Compose network",
			})
		}
	}
	if len(issues) == 0 {
		return nil
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path == issues[j].Path {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Path < issues[j].Path
	})
	return &CompatibilityError{Issues: issues}
}

func hasDeclaredTCPPort(service types.ServiceConfig) bool {
	for _, port := range service.Ports {
		if port.Target == 0 {
			continue
		}
		if protocolOrDefault(port.Protocol) == "TCP" {
			return true
		}
	}
	for _, token := range service.Expose {
		port, proto, err := parsePortToken(token)
		if err != nil || port <= 0 {
			continue
		}
		if protocolOrDefault(proto) == "TCP" {
			return true
		}
	}
	return false
}
