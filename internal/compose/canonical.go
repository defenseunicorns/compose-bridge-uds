package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	yamlv3 "gopkg.in/yaml.v3"

	"defenseunicorns/uds-compose-bridge/internal/model"
)

var invalidSettingPattern = regexp.MustCompile(`^invalid ([^:]+): (.+)$`)
var excludedServiceReferencePattern = regexp.MustCompile(`^(x-uds\.spec\.(?:network\.expose|monitor)) references excluded service "([^"]+)"$`)

func LoadCanonicalFile(path string) (model.App, error) {
	conversion, err := ConvertCanonicalFile(path)
	return conversion.App, err
}

func ConvertCanonicalFile(path string) (model.Conversion, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return rejectedConversion("compose", "read-error", fmt.Errorf("read canonical compose file: %w", err))
	}
	return convertCanonicalYAML(data, path)
}

func LoadCanonicalYAML(data []byte) (model.App, error) {
	conversion, err := ConvertCanonicalYAML(data)
	return conversion.App, err
}

func ConvertCanonicalYAML(data []byte) (model.Conversion, error) {
	return convertCanonicalYAML(data, "")
}

func convertCanonicalYAML(data []byte, sourcePath string) (model.Conversion, error) {
	var raw map[string]any
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return rejectedConversion("compose", "decode-error", fmt.Errorf("decode canonical compose extensions: %w", err))
	}
	if raw == nil {
		raw = map[string]any{}
	}

	configPath := sourcePath
	if configPath == "" {
		configPath = "-"
	}
	configDetails := types.ConfigDetails{
		WorkingDir: loadWorkingDir(sourcePath),
		ConfigFiles: []types.ConfigFile{{
			Filename: configPath,
			Content:  data,
		}},
		Environment: types.Mapping{},
	}

	project, err := loader.LoadWithContext(
		context.Background(),
		configDetails,
		loader.WithSkipValidation,
		func(opts *loader.Options) {
			// Bridge passes env_file references into the transformation container, but the
			// source files themselves are not available there. Keep canonical env_file
			// decoding without attempting to read those host paths.
			opts.SkipResolveEnvironment = true
		},
	)
	if err != nil {
		return rejectedConversion("compose", "decode-error", fmt.Errorf("decode canonical compose yaml: %w", err))
	}
	excludedServices := findExcludedServices(*project)
	report := buildConversionReport(*project, raw, excludedServices)
	if err := validateCompatibility(*project, raw, excludedServices); err != nil {
		var compatibilityErr *CompatibilityError
		if errors.As(err, &compatibilityErr) {
			decisions := make([]model.ConversionDecision, 0, len(compatibilityErr.Issues))
			for _, issue := range compatibilityErr.Issues {
				decisions = append(decisions, model.ConversionDecision{
					Path:        issue.Path,
					Code:        issue.Code,
					Message:     issue.Message,
					Remediation: issue.Remediation,
				})
			}
			report.RemoveConflictingTranslated(decisions)
			report.Rejected = append(report.Rejected, decisions...)
		}
		sortConversionReport(&report)
		return model.Conversion{Report: report}, err
	}

	app, err := loadProject(*project, raw, excludedServices)
	if err != nil {
		if decisions := loadProjectDecisions(err); len(decisions) > 0 {
			report.RemoveConflictingTranslated(decisions)
			report.Rejected = append(report.Rejected, decisions...)
		} else {
			report.Rejected = append(report.Rejected, model.ConversionDecision{
				Path:    "compose",
				Code:    "invalid-setting",
				Message: err.Error(),
			})
		}
		sortConversionReport(&report)
		return model.Conversion{Report: report}, err
	}
	completeConversionReport(&report, *project, raw, app)
	return model.Conversion{App: app, Report: report}, nil
}

func loadProjectDecisions(err error) []model.ConversionDecision {
	message := err.Error()
	if strings.HasPrefix(message, "unsupported fields:\n  - ") {
		rawPaths := strings.Split(strings.TrimPrefix(message, "unsupported fields:\n  - "), "\n  - ")
		decisions := make([]model.ConversionDecision, 0, len(rawPaths))
		for _, rawPath := range rawPaths {
			path, remediation := parseUnsupportedField(rawPath)
			decisions = append(decisions, model.ConversionDecision{
				Path:        path,
				Code:        "unsupported-field",
				Message:     "This x-uds field is not supported by the package translator.",
				Remediation: remediation,
			})
		}
		return decisions
	}

	matches := invalidSettingPattern.FindStringSubmatch(message)
	if len(matches) == 3 {
		path := matches[1]
		return []model.ConversionDecision{{
			Path:        path,
			Code:        "invalid-setting",
			Message:     matches[2],
			Remediation: remediationForInvalidSetting(path),
		}}
	}

	matches = excludedServiceReferencePattern.FindStringSubmatch(message)
	if len(matches) == 3 {
		path := matches[1]
		service := matches[2]
		return []model.ConversionDecision{{
			Path:        path,
			Code:        "invalid-setting",
			Message:     fmt.Sprintf("references excluded service %q", service),
			Remediation: remediationForExcludedServiceReference(path, service),
		}}
	}

	return nil
}

func parseUnsupportedField(rawPath string) (string, string) {
	rawPath = strings.TrimSpace(rawPath)
	parts := strings.SplitN(rawPath, " (", 2)
	path := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return path, "remove this field or replace it with a supported x-uds.metadata or x-uds.spec setting"
	}
	remediation := strings.TrimSuffix(parts[1], ")")
	return path, remediation
}

func remediationForInvalidSetting(path string) string {
	switch {
	case path == "x-uds.metadata":
		return "set x-uds.metadata to an object containing supported fields: name, version, labels, and annotations"
	case path == "x-uds.metadata.name":
		return "set x-uds.metadata.name to a lowercase DNS-1123-compatible value"
	case path == "x-uds.metadata.version":
		return "set x-uds.metadata.version to a non-empty string; use dev for development or <upstream>-uds.<sub-version> for a conventional release version"
	case strings.HasPrefix(path, "x-uds.metadata.labels"), strings.HasPrefix(path, "x-uds.metadata.annotations"):
		return "set this metadata field to an object whose values are strings"
	case path == "x-uds.spec":
		return "set x-uds.spec to an object containing supported fields: network, monitor, sso, and caBundle"
	case path == "x-uds.spec.network":
		return "set x-uds.spec.network to an object containing expose and allow arrays"
	case path == "x-uds.spec.network.expose", path == "x-uds.spec.network.allow", path == "x-uds.spec.monitor", path == "x-uds.spec.sso":
		return "set this field to an array"
	case path == "x-uds.spec.caBundle":
		return "set x-uds.spec.caBundle to an object containing configMap"
	case path == "x-uds.spec.caBundle.configMap":
		return "set x-uds.spec.caBundle.configMap to an object containing name, key, labels, and annotations"
	case strings.HasPrefix(path, "x-uds.spec.caBundle.configMap.labels"), strings.HasPrefix(path, "x-uds.spec.caBundle.configMap.annotations"):
		return "set this configMap metadata field to an object whose values are strings"
	case path == "x-uds.spec.caBundle.configMap.name", path == "x-uds.spec.caBundle.configMap.key":
		return "set this field to a non-empty string"
	default:
		return "correct the invalid x-uds setting so it matches the expected Compose extension schema"
	}
}

func remediationForExcludedServiceReference(path, service string) string {
	switch path {
	case "x-uds.spec.network.expose":
		return fmt.Sprintf("remove %q from x-uds.spec.network.expose or mark the dependency as required so the service is packaged", service)
	case "x-uds.spec.monitor":
		return fmt.Sprintf("remove %q from x-uds.spec.monitor or mark the dependency as required so the service is packaged", service)
	default:
		return "update this x-uds setting to reference only packaged services"
	}
}

// findExcludedServices identifies development-only dependencies. A service is
// excluded only when it is referenced by depends_on and every reference marks
// it as not required. Unreferenced services and services with any required
// reference remain part of the package.
func findExcludedServices(project types.Project) map[string]struct{} {
	type references struct {
		seen     bool
		required bool
	}

	dependencyReferences := map[string]references{}
	for _, service := range project.Services {
		for dependencyName, dependency := range service.DependsOn {
			refs := dependencyReferences[dependencyName]
			refs.seen = true
			refs.required = refs.required || dependency.Required
			dependencyReferences[dependencyName] = refs
		}
	}

	excluded := map[string]struct{}{}
	for serviceName := range project.Services {
		refs := dependencyReferences[serviceName]
		if refs.seen && !refs.required {
			excluded[serviceName] = struct{}{}
		}
	}
	return excluded
}

func loadWorkingDir(sourcePath string) string {
	if sourcePath != "" {
		return filepath.Dir(sourcePath)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func loadProject(project types.Project, raw map[string]any, excludedServices map[string]struct{}) (model.App, error) {
	if len(project.Services) == 0 {
		return model.App{}, fmt.Errorf("canonical compose model has no services")
	}

	projectNameRaw := strings.TrimSpace(project.Name)
	if projectNameRaw == "" {
		return model.App{}, fmt.Errorf("canonical compose model is missing name")
	}
	projectName, err := normalizeName(projectNameRaw)
	if err != nil {
		return model.App{}, fmt.Errorf("invalid compose project name: %w", err)
	}

	packageCfg, err := parsePackageConfig(projectName, raw)
	if err != nil {
		return model.App{}, err
	}

	networkAliases, err := normalizeTopLevelNetworks(project.Networks)
	if err != nil {
		return model.App{}, err
	}
	volumes, volumeAliases, err := normalizeTopLevelVolumes(project.Volumes)
	if err != nil {
		return model.App{}, err
	}
	secrets, secretAliases, err := normalizeTopLevelSecrets(project.Secrets)
	if err != nil {
		return model.App{}, err
	}
	excludedSecretRefs := collectExcludedSecretRefs(project, secretAliases, excludedServices)
	configs, configAliases, err := normalizeTopLevelConfigs(project.Configs)
	if err != nil {
		return model.App{}, err
	}

	keys := make([]string, 0, len(project.Services))
	for key := range project.Services {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	serviceAliases := map[string]string{}
	excludedAliases := map[string]struct{}{}
	for _, key := range keys {
		normalized, err := normalizeName(key)
		if err != nil {
			return model.App{}, fmt.Errorf("invalid service name %q: %w", key, err)
		}
		registerAlias(serviceAliases, key, normalized)
		registerAlias(serviceAliases, normalized, normalized)
		if _, excluded := excludedServices[key]; excluded {
			excludedAliases[key] = struct{}{}
			excludedAliases[normalized] = struct{}{}
		}
	}
	canonicalServices, canonicalSecrets, err := canonicalBuildMaps(project)
	if err != nil {
		return model.App{}, err
	}

	services := make([]model.Service, 0, len(keys))
	buildSecrets := map[string]any{}

	for _, key := range keys {
		if _, excluded := excludedServices[key]; excluded {
			continue
		}
		rawSvc := project.Services[key]
		serviceName, _ := resolveAlias(serviceAliases, key)

		ports, err := parsePorts(rawSvc.Ports, rawSvc.Expose)
		if err != nil {
			return model.App{}, fmt.Errorf("service %q ports: %w", key, err)
		}
		networks, err := parseServiceNetworks(rawSvc.Networks, networkAliases)
		if err != nil {
			return model.App{}, fmt.Errorf("service %q networks: %w", key, err)
		}
		volumeMounts, err := parseServiceVolumes(rawSvc.Volumes, serviceName, volumeAliases, volumes)
		if err != nil {
			return model.App{}, fmt.Errorf("service %q volumes: %w", key, err)
		}
		secretRefs, err := parseServiceSecrets(rawSvc.Secrets, secretAliases)
		if err != nil {
			return model.App{}, fmt.Errorf("service %q secrets: %w", key, err)
		}
		configRefs, err := parseServiceConfigs(rawSvc.Configs, configAliases)
		if err != nil {
			return model.App{}, fmt.Errorf("service %q configs: %w", key, err)
		}
		dependsOn, err := parseDependsOn(rawSvc.DependsOn, serviceAliases, excludedAliases)
		if err != nil {
			return model.App{}, fmt.Errorf("service %q depends_on: %w", key, err)
		}

		image := strings.TrimSpace(rawSvc.Image)
		var build *model.BuildDefinition
		if rawSvc.Build != nil {
			buildConfig, err := canonicalBuildConfig(canonicalServices, key, serviceAliases)
			if err != nil {
				return model.App{}, err
			}
			if err := rejectExcludedBuildContexts(key, buildConfig, excludedAliases); err != nil {
				return model.App{}, err
			}
			image = builtImageReference(packageCfg, serviceName)
			build = &model.BuildDefinition{
				Config:    buildConfig,
				ReadPaths: buildReadPaths(rawSvc.Build, project.Secrets),
			}
			for _, secret := range rawSvc.Build.Secrets {
				source := strings.TrimSpace(secret.Source)
				if source == "" {
					continue
				}
				definition, ok := canonicalSecrets[source]
				if !ok {
					return model.App{}, fmt.Errorf("build secret %q is missing from the canonical Compose model", source)
				}
				buildSecrets[source] = definition
			}
		}

		services = append(services, model.Service{
			Name:         serviceName,
			Image:        image,
			Build:        build,
			Ports:        ports,
			Networks:     networks,
			Env:          parseEnvironment(rawSvc.Environment),
			User:         strings.TrimSpace(rawSvc.User),
			Privileged:   rawSvc.Privileged,
			CapAdd:       rawSvc.CapAdd,
			CapDrop:      rawSvc.CapDrop,
			SecurityOpts: rawSvc.SecurityOpt,
			Command:      copyCommand(rawSvc.Entrypoint),
			Args:         copyCommand(rawSvc.Command),
			Stdin:        rawSvc.StdinOpen,
			Hostname:     strings.TrimSpace(rawSvc.Hostname),
			Healthcheck:  parseHealthcheck(rawSvc.HealthCheck),
			Volumes:      volumeMounts,
			Secrets:      secretRefs,
			Configs:      configRefs,
			DependsOn:    dependsOn,
			Resources:    parseResources(rawSvc.Deploy),
			Profiles:     normalizeProfiles(rawSvc.Profiles),
		})
	}

	markBoundarySecretsExternal(services, secrets, excludedSecretRefs)
	volumes, secrets, configs = retainReferencedResources(services, volumes, secrets, configs)
	if err := validateExcludedPackageReferences(packageCfg, excludedAliases); err != nil {
		return model.App{}, err
	}

	return model.App{
		Package:      packageCfg,
		Services:     services,
		Volumes:      volumes,
		Secrets:      secrets,
		Configs:      configs,
		BuildSecrets: buildSecrets,
	}, nil
}

// collectExcludedSecretRefs records known Compose secrets consumed by services
// omitted from the package. Invalid references on excluded services are ignored
// because those services otherwise bypass bridge compatibility validation.
func collectExcludedSecretRefs(project types.Project, aliases map[string]string, excludedServices map[string]struct{}) map[string]struct{} {
	refs := map[string]struct{}{}
	for serviceName, service := range project.Services {
		if _, excluded := excludedServices[serviceName]; !excluded {
			continue
		}
		for _, entry := range service.Secrets {
			name, ok := resolveAlias(aliases, strings.TrimSpace(entry.Source))
			if ok {
				refs[name] = struct{}{}
			}
		}
	}
	return refs
}

// markBoundarySecretsExternal keeps local Compose secret files available to
// excluded development dependencies while making packaged consumers reference
// a Kubernetes Secret supplied by the deployment environment.
func markBoundarySecretsExternal(services []model.Service, secrets map[string]model.Secret, excludedRefs map[string]struct{}) {
	for _, service := range services {
		for _, ref := range service.Secrets {
			if _, crossesBoundary := excludedRefs[ref.Source]; !crossesBoundary {
				continue
			}
			secret, exists := secrets[ref.Source]
			if !exists {
				continue
			}
			secret.External = true
			secrets[ref.Source] = secret
		}
	}
}

func rejectExcludedBuildContexts(serviceName string, build map[string]any, excluded map[string]struct{}) error {
	contexts, ok := asMap(build["additional_contexts"])
	if !ok {
		return nil
	}
	for _, value := range contexts {
		reference, ok := value.(string)
		if !ok || !strings.HasPrefix(reference, "service:") {
			continue
		}
		dependency := strings.TrimPrefix(reference, "service:")
		if _, isExcluded := excluded[dependency]; isExcluded {
			return fmt.Errorf("service %q build references excluded service %q through additional_contexts", serviceName, dependency)
		}
	}
	return nil
}

func retainReferencedResources(
	services []model.Service,
	volumes map[string]model.Volume,
	secrets map[string]model.Secret,
	configs map[string]model.Config,
) (map[string]model.Volume, map[string]model.Secret, map[string]model.Config) {
	volumeRefs := map[string]struct{}{}
	secretRefs := map[string]struct{}{}
	configRefs := map[string]struct{}{}
	for _, service := range services {
		for _, volume := range service.Volumes {
			volumeRefs[volume.Name] = struct{}{}
		}
		for _, secret := range service.Secrets {
			secretRefs[secret.Source] = struct{}{}
		}
		for _, config := range service.Configs {
			configRefs[config.Source] = struct{}{}
		}
	}

	retainedVolumes := map[string]model.Volume{}
	for name, volume := range volumes {
		if _, referenced := volumeRefs[name]; referenced {
			retainedVolumes[name] = volume
		}
	}
	retainedSecrets := map[string]model.Secret{}
	for name, secret := range secrets {
		if _, referenced := secretRefs[name]; referenced {
			retainedSecrets[name] = secret
		}
	}
	retainedConfigs := map[string]model.Config{}
	for name, config := range configs {
		if _, referenced := configRefs[name]; referenced {
			retainedConfigs[name] = config
		}
	}
	return retainedVolumes, retainedSecrets, retainedConfigs
}

func validateExcludedPackageReferences(pkg model.Package, excluded map[string]struct{}) error {
	for _, raw := range pkg.NetworkExpose {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		service := strings.TrimSpace(asString(entry["service"]))
		if _, isExcluded := excluded[service]; isExcluded {
			return fmt.Errorf("x-uds.spec.network.expose references excluded service %q", service)
		}
	}
	for _, raw := range pkg.Monitor {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		service := strings.TrimSpace(asString(entry["service"]))
		if _, isExcluded := excluded[service]; isExcluded {
			return fmt.Errorf("x-uds.spec.monitor references excluded service %q", service)
		}
	}
	return nil
}

func canonicalBuildMaps(project types.Project) (map[string]any, map[string]any, error) {
	data, err := project.MarshalYAML()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal canonical Compose build model: %w", err)
	}
	var canonical map[string]any
	if err := yamlv3.Unmarshal(data, &canonical); err != nil {
		return nil, nil, fmt.Errorf("decode canonical Compose build model: %w", err)
	}
	services, _ := asMap(canonical["services"])
	secrets, _ := asMap(canonical["secrets"])
	return services, secrets, nil
}

func canonicalBuildConfig(services map[string]any, serviceName string, aliases map[string]string) (map[string]any, error) {
	service, ok := asMap(services[serviceName])
	if !ok {
		return nil, fmt.Errorf("service %q is missing from the canonical Compose build model", serviceName)
	}
	build, ok := asMap(service["build"])
	if !ok {
		return nil, fmt.Errorf("service %q has no canonical Compose build definition", serviceName)
	}

	// Compose build.tags would add extra, user-controlled references to the OCI
	// archive. Built services always use the bridge-controlled internal image.
	delete(build, "tags")
	if contexts, ok := asMap(build["additional_contexts"]); ok {
		for name, value := range contexts {
			reference, ok := value.(string)
			if !ok || !strings.HasPrefix(reference, "service:") {
				continue
			}
			if resolved, ok := resolveAlias(aliases, strings.TrimPrefix(reference, "service:")); ok {
				contexts[name] = "service:" + resolved
			}
		}
	}
	return build, nil
}

func builtImageReference(pkg model.Package, serviceName string) string {
	return fmt.Sprintf("zarf.internal/%s-%s:%s", pkg.Name, serviceName, sanitizeImageTag(pkg.Version))
}

func buildReadPaths(build *types.BuildConfig, secrets types.Secrets) []string {
	paths := map[string]struct{}{}
	addBuildReadPath(paths, build.Context, false)
	for _, context := range build.AdditionalContexts {
		addBuildReadPath(paths, context, false)
	}
	if build.Dockerfile != "" {
		switch {
		case filepath.IsAbs(build.Dockerfile):
			addBuildReadPath(paths, build.Dockerfile, true)
		case isLocalBuildPath(build.Context):
			addBuildReadPath(paths, filepath.Join(build.Context, build.Dockerfile), true)
		}
	}
	for _, secret := range build.Secrets {
		if definition, ok := secrets[secret.Source]; ok {
			addBuildReadPath(paths, definition.File, true)
		}
	}
	for _, ssh := range build.SSH {
		addBuildReadPath(paths, ssh.Path, true)
	}

	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func addBuildReadPath(paths map[string]struct{}, value string, useParent bool) {
	if !isLocalBuildPath(value) {
		return
	}
	path := filepath.Clean(strings.TrimSpace(value))
	if useParent {
		path = filepath.Dir(path)
	}
	paths[path] = struct{}{}
}

func isLocalBuildPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "://") {
		return false
	}
	for _, prefix := range []string{"service:", "target:", "docker-image:", "oci-layout:"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	return true
}

var invalidImageTagChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeImageTag(value string) string {
	tag := invalidImageTagChars.ReplaceAllString(strings.TrimSpace(value), "-")
	tag = strings.TrimLeft(tag, ".-")
	if tag == "" {
		tag = "latest"
	}
	if len(tag) > 128 {
		tag = tag[:128]
	}
	return tag
}

func parsePackageConfig(projectName string, raw map[string]any) (model.Package, error) {
	config := model.Package{
		Name:            projectName,
		Namespace:       projectName,
		Group:           "compose",
		UpstreamVersion: model.DefaultUpstreamVersion,
		Version:         model.DefaultVersion,
	}

	rootUDS, ok := asMap(raw["x-uds"])
	if !ok {
		warnForNonConventionalPackageVersion(config.Version)
		return config, nil
	}
	keys := make([]string, 0, len(rootUDS))
	for key := range rootUDS {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	unsupported := make([]string, 0)
	legacyReplacements := map[string]string{
		"allow":    "x-uds.spec.network.allow",
		"caBundle": "x-uds.spec.caBundle",
		"monitor":  "x-uds.spec.monitor",
		"network":  "x-uds.spec.network",
		"sso":      "x-uds.spec.sso",
	}
	for _, key := range keys {
		if key == "metadata" || key == "spec" {
			continue
		}
		if key == "package" {
			legacyPackage, ok := asMap(rootUDS[key])
			if !ok || len(legacyPackage) == 0 {
				unsupported = append(unsupported, "x-uds.package (remove this field; use x-uds.metadata instead)")
				continue
			}
			packageKeys := make([]string, 0, len(legacyPackage))
			for packageKey := range legacyPackage {
				packageKeys = append(packageKeys, packageKey)
			}
			sort.Strings(packageKeys)
			for _, packageKey := range packageKeys {
				path := "x-uds.package." + packageKey
				switch packageKey {
				case "name":
					path += " (use x-uds.metadata.name)"
				case "version":
					path += " (use x-uds.metadata.version)"
				case "namespace":
					path += " (the default deployment namespace is derived from x-uds.metadata.name or the Compose project name)"
				case "group":
					path += " (remove this field; generated SSO client IDs use the compose group)"
				default:
					path += " (x-uds.package has been removed)"
				}
				unsupported = append(unsupported, path)
			}
			continue
		}
		path := "x-uds." + key
		if replacement, exists := legacyReplacements[key]; exists {
			path += " (use " + replacement + ")"
		}
		unsupported = append(unsupported, path)
	}
	if len(unsupported) > 0 {
		return model.Package{}, fmt.Errorf("unsupported fields:\n  - %s", strings.Join(unsupported, "\n  - "))
	}

	if rawMetadata, exists := rootUDS["metadata"]; exists {
		metadata, ok := asMap(rawMetadata)
		if !ok {
			return model.Package{}, fmt.Errorf("invalid x-uds.metadata: must be an object")
		}
		if value := strings.TrimSpace(asString(metadata["name"])); value != "" {
			normalized, err := normalizeName(value)
			if err != nil {
				return model.Package{}, fmt.Errorf("invalid x-uds.metadata.name: %w", err)
			}
			config.Name = normalized
			config.Namespace = normalized
		}
		if rawVersion, exists := metadata["version"]; exists {
			value, ok := rawVersion.(string)
			value = strings.TrimSpace(value)
			if !ok || value == "" {
				return model.Package{}, fmt.Errorf("invalid x-uds.metadata.version: must be a non-empty string")
			}
			upstreamVersion, packageVersion := normalizeConfiguredPackageVersion(value)
			config.UpstreamVersion = upstreamVersion
			config.Version = packageVersion
			config.VersionConfigured = true
		}
		if rawLabels, exists := metadata["labels"]; exists {
			labels, err := normalizeStringMap(rawLabels, "x-uds.metadata.labels")
			if err != nil {
				return model.Package{}, err
			}
			if len(labels) > 0 {
				config.Labels = labels
			}
		}
		if rawAnnotations, exists := metadata["annotations"]; exists {
			annotations, err := normalizeStringMap(rawAnnotations, "x-uds.metadata.annotations")
			if err != nil {
				return model.Package{}, err
			}
			if len(annotations) > 0 {
				config.Annotations = annotations
			}
		}
	}
	if !config.VersionConfigured {
		warnForNonConventionalPackageVersion(config.Version)
	}

	rawSpec, exists := rootUDS["spec"]
	if !exists {
		return config, nil
	}

	spec, ok := asMap(rawSpec)
	if !ok {
		return model.Package{}, fmt.Errorf("invalid x-uds.spec: must be an object")
	}
	if err := parseSpecConfig(&config, spec, "x-uds.spec"); err != nil {
		return model.Package{}, err
	}

	return config, nil
}

func parseSpecConfig(config *model.Package, spec map[string]any, path string) error {
	if rawNetwork, exists := spec["network"]; exists {
		network, ok := asMap(rawNetwork)
		if !ok {
			return fmt.Errorf("invalid %s.network: must be an object", path)
		}
		if rawExpose, exists := network["expose"]; exists {
			config.NetworkExposeConfigured = true
			expose, ok := asSlice(rawExpose)
			if !ok {
				return fmt.Errorf("invalid %s.network.expose: must be an array", path)
			}
			config.NetworkExpose = append(config.NetworkExpose, expose...)
		}
		if rawAllow, exists := network["allow"]; exists {
			allow, ok := asSlice(rawAllow)
			if !ok {
				return fmt.Errorf("invalid %s.network.allow: must be an array", path)
			}
			config.AdditionalAllow = append(config.AdditionalAllow, allow...)
		}
	}
	if rawMonitor, exists := spec["monitor"]; exists {
		config.MonitorConfigured = true
		monitor, ok := asSlice(rawMonitor)
		if !ok {
			return fmt.Errorf("invalid %s.monitor: must be an array", path)
		}
		config.Monitor = append(config.Monitor, monitor...)
	}

	if rawSSO, exists := spec["sso"]; exists {
		sso, ok := asSlice(rawSSO)
		if !ok {
			return fmt.Errorf("invalid %s.sso: must be an array", path)
		}
		config.SSOConfigured = true
		config.SSO = append(config.SSO, sso...)
	}

	if rawCABundle, exists := spec["caBundle"]; exists {
		caBundle, ok := asMap(rawCABundle)
		if !ok {
			return fmt.Errorf("invalid %s.caBundle: must be an object", path)
		}

		for key, value := range caBundle {
			if key != "configMap" {
				return fmt.Errorf("invalid %s.caBundle.%s: unsupported field", path, key)
			}
			configMap, err := normalizeCABundleConfigMap(value, path+".caBundle.configMap")
			if err != nil {
				return err
			}
			config.CABundle = map[string]any{"configMap": configMap}
		}
	}

	return nil
}

var (
	upstreamVersionPattern = regexp.MustCompile(`^[vV]?([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?(-[0-9A-Za-z.-]+)?$`)
	udsVersionPattern      = regexp.MustCompile(`^(.+)-uds\.([0-9]+)$`)
)

func normalizeConfiguredPackageVersion(value string) (string, string) {
	if value == model.DevelopmentVersion {
		warnForNonConventionalPackageVersion(value)
		return model.DevelopmentVersion, model.DevelopmentVersion
	}

	if matches := udsVersionPattern.FindStringSubmatch(value); matches != nil {
		upstream, ok := normalizeUpstreamVersion(matches[1])
		if ok && (len(matches[2]) == 1 || !strings.HasPrefix(matches[2], "0")) {
			return upstream, value
		}
	}

	warnForNonConventionalPackageVersion(value)
	upstream, ok := normalizeUpstreamVersion(value)
	if !ok {
		upstream = value
	}
	return upstream, value
}

func warnForNonConventionalPackageVersion(value string) {
	fmt.Fprintf(os.Stderr, "warning: x-uds.metadata.version %q does not match <upstream>-uds.<sub-version>; using the value unchanged\n", value)
}

func normalizeUpstreamVersion(value string) (string, bool) {
	matches := upstreamVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if matches == nil || !validPrerelease(matches[4]) {
		return "", false
	}

	parts := make([]string, 3)
	for i := range parts {
		raw := matches[i+1]
		if raw == "" {
			raw = "0"
		}
		number, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return "", false
		}
		parts[i] = strconv.FormatUint(number, 10)
	}
	return strings.Join(parts, ".") + matches[4], true
}

func validPrerelease(value string) bool {
	if value == "" {
		return true
	}
	for _, identifier := range strings.Split(strings.TrimPrefix(value, "-"), ".") {
		if identifier == "" {
			return false
		}
		if len(identifier) > 1 && identifier[0] == '0' {
			allNumeric := true
			for _, character := range identifier {
				if character < '0' || character > '9' {
					allNumeric = false
					break
				}
			}
			if allNumeric {
				return false
			}
		}
	}
	return true
}

func normalizeCABundleConfigMap(value any, field string) (map[string]any, error) {
	configMap, ok := asMap(value)
	if !ok {
		return nil, fmt.Errorf("invalid %s: must be an object", field)
	}

	normalized := map[string]any{}
	for key, raw := range configMap {
		switch key {
		case "name", "key":
			value := strings.TrimSpace(asString(raw))
			if value == "" {
				return nil, fmt.Errorf("invalid %s.%s: must be a non-empty string", field, key)
			}
			normalized[key] = value
		case "labels", "annotations":
			value, err := normalizeStringMap(raw, fmt.Sprintf("%s.%s", field, key))
			if err != nil {
				return nil, err
			}
			normalized[key] = value
		default:
			return nil, fmt.Errorf("invalid %s.%s: unsupported field", field, key)
		}
	}

	return normalized, nil
}

func normalizeStringMap(value any, field string) (map[string]string, error) {
	items, ok := asMap(value)
	if !ok {
		return nil, fmt.Errorf("invalid %s: must be an object", field)
	}

	normalized := make(map[string]string, len(items))
	for key, raw := range items {
		text, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("invalid %s.%s: must be a string", field, key)
		}
		normalized[key] = text
	}
	return normalized, nil
}

func parseEnvironment(raw types.MappingWithEquals) []model.EnvVar {
	if len(raw) == 0 {
		return nil
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]model.EnvVar, 0, len(keys))
	for _, key := range keys {
		value := ""
		if raw[key] != nil {
			value = *raw[key]
		}
		env = append(env, model.EnvVar{Name: key, Value: value})
	}
	return env
}

func copyCommand(command types.ShellCommand) []string {
	if len(command) == 0 {
		return nil
	}
	out := make([]string, 0, len(command))
	for _, token := range command {
		out = append(out, token)
	}
	return out
}

func parseHealthcheck(raw *types.HealthCheckConfig) *model.Healthcheck {
	if raw == nil || raw.Disable || len(raw.Test) == 0 {
		return nil
	}
	tokens := []string(raw.Test)
	mode := strings.ToUpper(strings.TrimSpace(tokens[0]))
	args := tokens[1:]

	command := []string{}
	switch mode {
	case "NONE":
		return nil
	case "CMD-SHELL":
		if len(args) == 0 {
			return nil
		}
		command = []string{"/bin/sh", "-c", strings.Join(args, " ")}
	case "CMD":
		if len(args) == 0 {
			return nil
		}
		command = append(command, args...)
	default:
		command = append(command, tokens...)
	}

	probe := &model.Healthcheck{Command: command}
	if raw.Interval != nil {
		probe.PeriodSeconds = durationSeconds(*raw.Interval)
	}
	if raw.StartPeriod != nil {
		probe.InitialDelaySeconds = durationSeconds(*raw.StartPeriod)
	}
	if raw.Timeout != nil {
		probe.TimeoutSeconds = durationSeconds(*raw.Timeout)
	}
	if raw.Retries != nil {
		probe.FailureThreshold = int(*raw.Retries)
	}
	return probe
}

func durationSeconds(value types.Duration) int {
	seconds := int(time.Duration(value).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

func parsePorts(rawPorts []types.ServicePortConfig, expose types.StringOrNumberList) ([]model.Port, error) {
	ports := []model.Port{}
	seen := map[string]struct{}{}
	for _, raw := range rawPorts {
		if raw.Target == 0 {
			continue
		}
		proto := protocolOrDefault(raw.Protocol)
		key := portKey(int(raw.Target), proto)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		ports = append(ports, model.Port{
			Number:      int(raw.Target),
			Protocol:    proto,
			Raw:         key,
			Published:   true,
			Name:        strings.TrimSpace(raw.Name),
			AppProtocol: strings.TrimSpace(raw.AppProtocol),
		})
	}

	for _, token := range expose {
		target, proto, err := parsePortToken(token)
		if err != nil {
			return nil, err
		}
		key := portKey(target, proto)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		ports = append(ports, model.Port{Number: target, Protocol: proto, Raw: key})
	}

	return ports, nil
}

func portKey(number int, proto string) string {
	return fmt.Sprintf("%d/%s", number, protocolOrDefault(proto))
}

func parsePortToken(token string) (int, string, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return 0, "", fmt.Errorf("port must not be empty")
	}

	proto := "TCP"
	base := trimmed
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		base = strings.TrimSpace(trimmed[:idx])
		proto = protocolOrDefault(trimmed[idx+1:])
	}

	parts := strings.Split(base, ":")
	target := strings.TrimSpace(parts[len(parts)-1])
	if strings.Contains(target, "-") {
		rangeParts := strings.SplitN(target, "-", 2)
		target = strings.TrimSpace(rangeParts[0])
	}
	port, err := strconv.Atoi(target)
	if err != nil || port <= 0 {
		return 0, "", fmt.Errorf("invalid port %q", token)
	}
	return port, proto, nil
}

func protocolOrDefault(raw string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == "" {
		return "TCP"
	}
	return trimmed
}

func parseServiceVolumes(raw []types.ServiceVolumeConfig, serviceName string, aliases map[string]string, volumes map[string]model.Volume) ([]model.VolumeMount, error) {
	out := make([]model.VolumeMount, 0, len(raw))
	for _, mount := range raw {
		mountType := strings.ToLower(strings.TrimSpace(mount.Type))
		source := strings.TrimSpace(mount.Source)
		target := strings.TrimSpace(mount.Target)
		if target == "" {
			return nil, fmt.Errorf("volume.target must be set")
		}
		if mountType == "" {
			if looksLikePath(source) {
				mountType = "bind"
			} else {
				mountType = "volume"
			}
		}

		switch mountType {
		case "bind":
			fmt.Fprintf(os.Stderr, "warning: service %q bind mount %q:%q skipped; bind mounts have no equivalent in Kubernetes\n", serviceName, source, target)
			continue
		case "volume":
			name := strings.TrimSpace(source)
			if name == "" {
				name = anonymousVolumeName(serviceName, target)
				volumes[name] = model.Volume{Name: name}
				registerAlias(aliases, name, name)
			} else {
				resolved, ok := resolveAlias(aliases, name)
				if !ok {
					return nil, fmt.Errorf("unknown top-level volume %q", source)
				}
				name = resolved
			}
			out = append(out, model.VolumeMount{Name: name, Target: target, ReadOnly: mount.ReadOnly})
		default:
			return nil, fmt.Errorf("service %q mount %q uses unsupported type %q; only named volumes are supported", serviceName, target, mountType)
		}
	}
	return out, nil
}

func parseServiceSecrets(raw []types.ServiceSecretConfig, aliases map[string]string) ([]model.FileRef, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	refs := make([]model.FileRef, 0, len(raw))
	seen := map[string]struct{}{}
	for _, entry := range raw {
		sourceRaw := strings.TrimSpace(entry.Source)
		if sourceRaw == "" {
			return nil, fmt.Errorf("secret source must not be empty")
		}
		source, ok := resolveAlias(aliases, sourceRaw)
		if !ok {
			return nil, fmt.Errorf("unknown top-level secret %q", sourceRaw)
		}
		target := strings.TrimSpace(entry.Target)
		key := source + "::" + target
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, model.FileRef{Source: source, Target: target})
	}
	return refs, nil
}

func parseServiceConfigs(raw []types.ServiceConfigObjConfig, aliases map[string]string) ([]model.FileRef, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	refs := make([]model.FileRef, 0, len(raw))
	seen := map[string]struct{}{}
	for _, entry := range raw {
		sourceRaw := strings.TrimSpace(entry.Source)
		if sourceRaw == "" {
			return nil, fmt.Errorf("config source must not be empty")
		}
		source, ok := resolveAlias(aliases, sourceRaw)
		if !ok {
			return nil, fmt.Errorf("unknown top-level config %q", sourceRaw)
		}
		target := strings.TrimSpace(entry.Target)
		var mode *int32
		if entry.Mode != nil {
			value := int64(*entry.Mode)
			if value < 0 || value > 0o777 {
				return nil, fmt.Errorf("config %q mode %s must be between 0000 and 0777", sourceRaw, entry.Mode.String())
			}
			converted := int32(value)
			mode = &converted
		}
		key := source + "::" + target
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, model.FileRef{Source: source, Target: target, Mode: mode})
	}
	return refs, nil
}

func parseDependsOn(raw types.DependsOnConfig, serviceAliases map[string]string, excludedServices map[string]struct{}) ([]model.Dependency, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	deps := make([]model.Dependency, 0, len(raw))
	seen := map[string]struct{}{}
	for rawName, dep := range raw {
		resolved, ok := resolveAlias(serviceAliases, rawName)
		if !ok {
			return nil, fmt.Errorf("unknown service %q", rawName)
		}
		if _, excluded := excludedServices[resolved]; excluded {
			continue
		}
		if _, exists := seen[resolved]; exists {
			continue
		}
		seen[resolved] = struct{}{}
		condition := strings.TrimSpace(dep.Condition)
		if condition == "" {
			condition = types.ServiceConditionStarted
		}
		deps = append(deps, model.Dependency{Service: resolved, Condition: condition})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Service < deps[j].Service })
	return deps, nil
}

func parseServiceNetworks(raw map[string]*types.ServiceNetworkConfig, aliases map[string]string) ([]string, error) {
	networks := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for rawName := range raw {
		name, ok := resolveAlias(aliases, rawName)
		if !ok {
			return nil, fmt.Errorf("unknown network %q", rawName)
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		networks = append(networks, name)
	}
	sort.Strings(networks)
	return networks, nil
}

func parseResources(raw *types.DeployConfig) model.Resources {
	if raw == nil {
		return model.Resources{}
	}
	var out model.Resources
	if raw.Resources.Limits != nil {
		out.Limits.CPU = formatNanoCPUs(raw.Resources.Limits.NanoCPUs)
		out.Limits.Memory = formatUnitBytes(raw.Resources.Limits.MemoryBytes)
	}
	if raw.Resources.Reservations != nil {
		out.Requests.CPU = formatNanoCPUs(raw.Resources.Reservations.NanoCPUs)
		out.Requests.Memory = formatUnitBytes(raw.Resources.Reservations.MemoryBytes)
	}
	return out
}

func normalizeTopLevelVolumes(raw types.Volumes) (map[string]model.Volume, map[string]string, error) {
	volumes := map[string]model.Volume{}
	aliases := map[string]string{}
	for key, value := range raw {
		normalized, err := normalizeName(key)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid top-level volume name %q: %w", key, err)
		}
		if _, exists := volumes[normalized]; exists {
			return nil, nil, fmt.Errorf("duplicate normalized top-level volume name %q", normalized)
		}
		volumes[normalized] = model.Volume{Name: normalized, External: bool(value.External)}
		registerAlias(aliases, key, normalized)
		registerAlias(aliases, normalized, normalized)
		if strings.TrimSpace(value.Name) != "" {
			registerAlias(aliases, value.Name, normalized)
		}
	}
	return volumes, aliases, nil
}

func normalizeTopLevelNetworks(raw types.Networks) (map[string]string, error) {
	aliases := map[string]string{}
	seen := map[string]struct{}{}
	for key, network := range raw {
		normalized, err := normalizeName(key)
		if err != nil {
			return nil, fmt.Errorf("invalid top-level network name %q: %w", key, err)
		}
		if _, exists := seen[normalized]; exists {
			return nil, fmt.Errorf("duplicate normalized top-level network name %q", normalized)
		}
		seen[normalized] = struct{}{}
		registerAlias(aliases, key, normalized)
		registerAlias(aliases, normalized, normalized)
		if bool(network.External) {
			fmt.Fprintf(os.Stderr, "warning: external Compose network %q treated as package-local; declare access to external workloads with x-uds.spec.network.allow\n", key)
		}
	}
	return aliases, nil
}

func normalizeTopLevelSecrets(raw types.Secrets) (map[string]model.Secret, map[string]string, error) {
	secrets := map[string]model.Secret{}
	candidates := make([]namedResourceAlias, 0, len(raw))
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := raw[key]
		normalized, err := normalizeName(key)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid top-level secret name %q: %w", key, err)
		}
		if _, exists := secrets[normalized]; exists {
			return nil, nil, fmt.Errorf("duplicate normalized top-level secret name %q", normalized)
		}
		secrets[normalized] = model.Secret{Name: normalized, External: bool(value.External)}
		candidates = append(candidates, namedResourceAlias{Key: key, Normalized: normalized, PlatformName: value.Name})
	}
	aliases, err := buildNamedResourceAliases("secret", candidates)
	if err != nil {
		return nil, nil, err
	}
	return secrets, aliases, nil
}

func normalizeTopLevelConfigs(raw types.Configs) (map[string]model.Config, map[string]string, error) {
	configs := map[string]model.Config{}
	aliases := map[string]string{}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := raw[key]
		normalized, err := normalizeName(key)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid top-level config name %q: %w", key, err)
		}
		if _, exists := configs[normalized]; exists {
			return nil, nil, fmt.Errorf("duplicate normalized top-level config name %q", normalized)
		}
		configs[normalized] = model.Config{
			Name:         normalized,
			ComposeName:  strings.TrimSpace(key),
			ExternalName: strings.TrimSpace(value.Name),
			External:     bool(value.External),
			Content:      value.Content,
		}
		registerAlias(aliases, key, normalized)
		registerAlias(aliases, normalized, normalized)
	}
	return configs, aliases, nil
}

func formatNanoCPUs(value types.NanoCPUs) string {
	v := value.Value()
	if v <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(v), 'f', -1, 32)
}

func formatUnitBytes(value types.UnitBytes) string {
	bytes := int64(value)
	if bytes <= 0 {
		return ""
	}

	// Prefer the largest exact Kubernetes binary quantity. Keeping the value
	// exact avoids silently changing the Compose limit or reservation.
	units := []struct {
		suffix string
		bytes  int64
	}{
		{suffix: "Ei", bytes: 1 << 60},
		{suffix: "Pi", bytes: 1 << 50},
		{suffix: "Ti", bytes: 1 << 40},
		{suffix: "Gi", bytes: 1 << 30},
		{suffix: "Mi", bytes: 1 << 20},
		{suffix: "Ki", bytes: 1 << 10},
	}
	for _, unit := range units {
		if bytes%unit.bytes == 0 {
			return strconv.FormatInt(bytes/unit.bytes, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes, 10)
}

func normalizeProfiles(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, profile := range raw {
		trimmed := strings.TrimSpace(profile)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func looksLikePath(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") || strings.HasPrefix(trimmed, "~/") {
		return true
	}
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return true
	}
	return strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\")
}

func anonymousVolumeName(serviceName string, target string) string {
	base := strings.TrimSpace(target)
	if base == "" {
		base = "data"
	}
	base = strings.TrimPrefix(base, "/")
	base = strings.ReplaceAll(base, "/", "-")
	base = strings.ReplaceAll(base, "_", "-")
	base = strings.ReplaceAll(base, ".", "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "data"
	}
	name, err := normalizeName(fmt.Sprintf("%s-%s", serviceName, base))
	if err != nil {
		return "volume"
	}
	return name
}

type namedResourceAlias struct {
	Key          string
	Normalized   string
	PlatformName string
}

// buildNamedResourceAliases gives Compose's logical top-level keys precedence
// over optional platform resource names.
func buildNamedResourceAliases(resourceKind string, candidates []namedResourceAlias) (map[string]string, error) {
	aliases := map[string]string{}
	directAliases := map[string]struct{}{}
	for _, candidate := range candidates {
		for _, raw := range []string{candidate.Key, candidate.Normalized} {
			trimmed := strings.TrimSpace(raw)
			if trimmed == "" {
				continue
			}
			registerAlias(aliases, trimmed, candidate.Normalized)
			directAliases[trimmed] = struct{}{}
		}
	}

	for _, candidate := range candidates {
		platformName := strings.TrimSpace(candidate.PlatformName)
		if platformName == "" {
			continue
		}
		if _, direct := directAliases[platformName]; direct {
			continue
		}
		if existing, exists := aliases[platformName]; exists && existing != candidate.Normalized {
			return nil, fmt.Errorf(
				"ambiguous top-level %s platform name %q resolves to both %q and %q",
				resourceKind,
				platformName,
				existing,
				candidate.Normalized,
			)
		}
		registerAlias(aliases, platformName, candidate.Normalized)
	}
	return aliases, nil
}

func registerAlias(aliases map[string]string, raw string, normalized string) {
	trimmedRaw := strings.TrimSpace(raw)
	if trimmedRaw == "" {
		return
	}
	trimmedNormalized := strings.TrimSpace(normalized)
	if trimmedNormalized == "" {
		return
	}
	aliases[trimmedRaw] = trimmedNormalized
}

func resolveAlias(aliases map[string]string, raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if resolved, ok := aliases[trimmed]; ok {
		return resolved, true
	}
	normalized, err := normalizeName(trimmed)
	if err != nil {
		return "", false
	}
	if resolved, ok := aliases[normalized]; ok {
		return resolved, true
	}
	return "", false
}

var validNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func normalizeName(name string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	trimmed = strings.ReplaceAll(trimmed, "_", "-")
	if !validNamePattern.MatchString(trimmed) {
		return "", fmt.Errorf("name %q must match %s", name, validNamePattern.String())
	}
	return trimmed, nil
}

func asMap(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

func asSlice(value any) ([]any, bool) {
	v, ok := value.([]any)
	return v, ok
}

func asString(value any) string {
	s, _ := value.(string)
	return s
}

func asInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func asStringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, entry := range values {
		if s := strings.TrimSpace(asString(entry)); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func resolveRelativePath(baseDir string, path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Clean(filepath.Join(home, strings.TrimPrefix(trimmed, "~/")))
		}
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed)
	}
	return filepath.Clean(filepath.Join(baseDir, trimmed))
}
