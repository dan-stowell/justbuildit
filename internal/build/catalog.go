package build

import (
	"fmt"
	"sort"
	"strings"
)

// Toolchain is a short name for a commonly used build image.
type Toolchain struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// DefaultImage is used when a request names neither an image nor a toolchain.
const DefaultImage = "buildpack-deps:bookworm"

var toolchains = map[string]string{
	"go":     "golang:1.25",
	"node":   "node:22",
	"python": "python:3.13",
	"rust":   "rust:1",
	"c":      "gcc:14",
	"cpp":    "gcc:14",
	"java":   "maven:3-eclipse-temurin-21",
}

// Toolchains returns the catalog sorted by name.
func Toolchains() []Toolchain {
	var out []Toolchain
	for name, image := range toolchains {
		out = append(out, Toolchain{Name: name, Image: image})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResolveImage picks the image for a request: an explicit image wins, then a
// toolchain short name, then DefaultImage.
func ResolveImage(image, toolchain string) (string, error) {
	if image = strings.TrimSpace(image); image != "" {
		return image, nil
	}
	toolchain = strings.ToLower(strings.TrimSpace(toolchain))
	if toolchain == "" {
		return DefaultImage, nil
	}
	if img, ok := toolchains[toolchain]; ok {
		return img, nil
	}
	var names []string
	for _, tc := range Toolchains() {
		names = append(names, tc.Name)
	}
	return "", fmt.Errorf("unknown toolchain %q (known: %s; or pass any docker image as \"image\")", toolchain, strings.Join(names, ", "))
}
