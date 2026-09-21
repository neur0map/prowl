package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/neur0map/prowl/internal/capability"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/knowledge"
)

// Surface selects MCP tool registration without changing legacy schemas.
type Surface string

const (
	SurfaceLegacy Surface = "legacy"
	SurfaceCore   Surface = "core"
	SurfaceAll    Surface = "all"
)

// ParseSurface validates an MCP compatibility surface selector.
func ParseSurface(value string) (Surface, error) {
	surface := Surface(strings.ToLower(strings.TrimSpace(value)))
	switch surface {
	case SurfaceLegacy, SurfaceCore, SurfaceAll:
		return surface, nil
	default:
		return "", fmt.Errorf("unknown MCP surface %q (want legacy, core, or all)", value)
	}
}

// ServerOptions supplies shared v2 services and workspace paths.
type ServerOptions struct {
	Surface      Surface
	Context      *contextpacket.Service
	Knowledge    *knowledge.Repository
	Capabilities *capability.Catalog
	Root         string
	BeforeCall   func(context.Context) error
}

func (options *ServerOptions) normalize() {
	if options.Surface == "" {
		options.Surface = SurfaceLegacy
	}
}
