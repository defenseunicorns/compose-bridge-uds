package images

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"defenseunicorns/uds-compose-bridge/internal/images/wait"
	"defenseunicorns/uds-compose-bridge/internal/model"
)

const waitDir = "images/wait"

// WaitTarget describes the package-local wait image's build.
type WaitTarget struct {
	Name   string
	Image  string
	Config map[string]any
}

// WaitPort selects the dependency's first declared TCP port.
func WaitPort(ports []model.Port) (int, bool) {
	for _, port := range ports {
		if port.Number > 0 && strings.EqualFold(strings.TrimSpace(port.Protocol), "TCP") {
			return port.Number, true
		}
	}
	return 0, false
}

// HasWaits reports whether any included service needs a dependency wait.
func HasWaits(app model.App) bool {
	ports := map[string]int{}
	for _, svc := range app.Services {
		if port, ok := WaitPort(svc.Ports); ok {
			ports[svc.Name] = port
		}
	}
	for _, svc := range app.Services {
		for _, dep := range svc.DependsOn {
			if ports[dep.Service] > 0 {
				return true
			}
		}
	}
	return false
}

var invalidWaitImageTagChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func waitImageTag(version string) string {
	tag := strings.TrimLeft(invalidWaitImageTagChars.ReplaceAllString(strings.TrimSpace(version), "-"), ".-")
	if tag == "" {
		tag = "latest"
	}
	if len(tag) > 128 {
		tag = tag[:128]
	}
	return tag
}

// WaitBuild chooses a deterministic target and image without colliding with services.
func WaitBuild(app model.App) WaitTarget {
	// Include prebuilt service names and image references in the collision check.
	for suffix := 0; ; suffix++ {
		name := "compose-bridge-dependency-wait"
		if suffix > 0 {
			name += fmt.Sprintf("-%d", suffix)
		}
		image := fmt.Sprintf("zarf.internal/%s/%s:%s", app.Package.Name, name, waitImageTag(app.Package.Version))
		collision := false
		for _, svc := range app.Services {
			if svc.Name == name || svc.Image == image {
				collision = true
				break
			}
		}
		if !collision {
			return WaitTarget{Name: name, Image: image, Config: map[string]any{"context": "./" + waitDir}}
		}
	}
}

// WriteWaitContext writes the inspectable source and Dockerfile under the package root.
func WriteWaitContext(root string) error {
	dir := filepath.Join(root, waitDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dependency wait context: %w", err)
	}
	for name, data := range map[string][]byte{"Dockerfile": wait.Dockerfile, "main.go": wait.Source} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return fmt.Errorf("write dependency wait %s: %w", name, err)
		}
	}
	return nil
}
