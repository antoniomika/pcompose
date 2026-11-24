package compose

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// project is the part of `docker compose config --format json` pcompose reads.
type project struct {
	Name     string             `json:"name"`
	Services map[string]service `json:"services"`
}

type service struct {
	Restart string  `json:"restart"`
	Deploy  *deploy `json:"deploy"`
}

type deploy struct {
	RestartPolicy any `json:"restart_policy"`
}

func parseProject(config string) (*project, error) {
	var p project
	if err := json.Unmarshal([]byte(config), &p); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}

	return &p, nil
}

// defaultRestart returns the services without a restart policy, which
// pcompose gives DefaultRestartPolicy. Compose applies deploy.restart_policy
// when restart is unset.
func (p *project) defaultRestart() []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		svc := p.Services[name]
		if svc.Restart == "" && (svc.Deploy == nil || svc.Deploy.RestartPolicy == nil) {
			names = append(names, name)
		}
	}

	return names
}
