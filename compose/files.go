// Package compose deploys and operates projects with the docker CLI, so it
// works with whichever engine DOCKER_HOST or a docker context picks.
package compose

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

var (
	baseFiles     = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	overrideFiles = []string{"compose.override.yaml", "compose.override.yml", "docker-compose.override.yaml", "docker-compose.override.yml"}
	pcomposeFiles = []string{"compose.pcompose.yaml", "compose.pcompose.yml", "docker-compose.pcompose.yaml", "docker-compose.pcompose.yml", "pcompose.yaml", "pcompose.yml"}
)

// dotEnvSets reports whether dir's .env sets a variable. Its value is left to
// compose, which knows how to read it.
func dotEnvSets(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return false
	}

	return regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?` + regexp.QuoteMeta(name) + `[ \t]*[=:]`).Match(data)
}

// Files picks the compose files dir's project deploys with: the first base
// file, the first override file, and every pcompose file that exists.
func Files(dir string) []string {
	exists := func(f string) bool {
		info, err := os.Stat(filepath.Join(dir, f))
		return err == nil && !info.IsDir()
	}

	var files []string
	for _, group := range [][]string{baseFiles, overrideFiles} {
		if i := slices.IndexFunc(group, exists); i >= 0 {
			files = append(files, group[i])
		}
	}

	for _, f := range pcomposeFiles {
		if exists(f) {
			files = append(files, f)
		}
	}

	return files
}
