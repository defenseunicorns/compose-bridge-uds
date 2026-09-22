package model

import (
	"fmt"
	"strings"
)

const (
	DevelopmentVersion      = "dev"
	DevelopmentChartVersion = "0.0.0-dev"
	DefaultUpstreamVersion  = DevelopmentVersion
	DefaultVersion          = DevelopmentVersion
	DependencyInitImage     = "docker.io/library/busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
)

type Port struct {
	Number      int
	Protocol    string
	Raw         string
	Published   bool
	Name        string
	AppProtocol string
}

func (p Port) HasWebHint() bool {
	return IsWebPortHint(p.AppProtocol) || IsWebPortHint(p.Name)
}

func IsWebPortHint(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		normalized = normalized[idx+1:]
	}
	normalized = strings.ReplaceAll(normalized, "_", "-")

	switch normalized {
	case "http", "https", "http2", "h2c", "grpc", "grpcs", "web", "www":
		return true
	default:
		return false
	}
}

type EnvVar struct {
	Name  string
	Value string
}

type ResourceSet struct {
	CPU    string
	Memory string
}

type Resources struct {
	Limits   ResourceSet
	Requests ResourceSet
}

type Healthcheck struct {
	Command             []string
	PeriodSeconds       int
	InitialDelaySeconds int
	TimeoutSeconds      int
	FailureThreshold    int
}

type Volume struct {
	Name     string
	External bool
}

type VolumeMount struct {
	Name     string
	Target   string
	ReadOnly bool
}

type FileRef struct {
	Source string
	Target string
	Mode   *int32
}

type Secret struct {
	Name     string
	External bool
}

type Config struct {
	Name         string
	ExternalName string
	External     bool
	Content      string
}

type Dependency struct {
	Service   string
	Condition string
}

type Package struct {
	Name                    string
	Namespace               string
	Group                   string
	UpstreamVersion         string
	Version                 string
	VersionConfigured       bool
	Labels                  map[string]string
	Annotations             map[string]string
	NetworkExposeConfigured bool
	NetworkExpose           []any
	MonitorConfigured       bool
	Monitor                 []any
	AdditionalAllow         []any
	SSOConfigured           bool
	SSO                     []any
	CABundle                map[string]any
}

type BuildDefinition struct {
	// Config keeps the canonical Compose build fields as generic YAML data so
	// new Compose build options can pass through without duplicating its schema.
	Config map[string]any
	// ReadPaths lists host directories Buildx Bake must be allowed to access.
	ReadPaths []string
}

type Service struct {
	Name         string
	Image        string
	Build        *BuildDefinition
	Ports        []Port
	Networks     []string
	Env          []EnvVar
	User         string
	Privileged   bool
	CapAdd       []string
	CapDrop      []string
	SecurityOpts []string
	Command      []string
	Args         []string
	Stdin        bool
	Hostname     string
	Healthcheck  *Healthcheck
	Volumes      []VolumeMount
	Secrets      []FileRef
	Configs      []FileRef
	DependsOn    []Dependency
	Resources    Resources
	Profiles     []string
}

type App struct {
	Package      Package
	Services     []Service
	Volumes      map[string]Volume
	Secrets      map[string]Secret
	Configs      map[string]Config
	BuildSecrets map[string]any
}

type ConversionDecision struct {
	Path        string `json:"path"`
	Target      string `json:"target,omitempty"`
	Value       string `json:"value,omitempty"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type ConversionReport struct {
	Translated []ConversionDecision `json:"translated"`
	Inferred   []ConversionDecision `json:"inferred"`
	Ignored    []ConversionDecision `json:"ignored"`
	Rejected   []ConversionDecision `json:"rejected"`
}

// RemoveConflictingTranslated removes translated decisions that overlap a
// rejected setting at the same path or at a parent or child path.
func (r *ConversionReport) RemoveConflictingTranslated(decisions []ConversionDecision) {
	filtered := r.Translated[:0]
	for _, translated := range r.Translated {
		conflict := false
		for _, decision := range decisions {
			if decisionPathsOverlap(translated.Path, decision.Path) {
				conflict = true
				break
			}
		}
		if !conflict {
			filtered = append(filtered, translated)
		}
	}
	r.Translated = filtered
}

func decisionPathsOverlap(left, right string) bool {
	return left == right || decisionPathContains(left, right) || decisionPathContains(right, left)
}

func decisionPathContains(parent, child string) bool {
	if !strings.HasPrefix(child, parent) || len(child) == len(parent) {
		return false
	}
	separator := child[len(parent)]
	return separator == '.' || separator == '['
}

type Conversion struct {
	App    App
	Report ConversionReport
}

func (s Service) PrimaryPort() (Port, error) {
	if len(s.Ports) == 0 {
		return Port{}, fmt.Errorf("service %q has no declared ports", s.Name)
	}
	return s.Ports[0], nil
}
