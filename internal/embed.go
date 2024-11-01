package internal

import (
	"embed"
	"strings"
)

//go:embed embed
var embedFs embed.FS

// Gets the operator version by reading the embedded version.txt file
func GetOperatorVersion() string {
	vb, err := embedFs.ReadFile("embed/version.txt")
	if err != nil {
		return "0.0.0+undefined"
	}
	return strings.TrimSpace(string(vb))
}
