package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neur0map/prowl/internal/query"
)

const (
	maxSourceResourceBytes = 2 << 20
	maxGitOutputBytes      = 2 << 20
)

func registerResources(server *sdk.Server, h *handlers) {
	server.AddResource(&sdk.Resource{URI: "prowl://workspaces", Name: "workspaces", Title: "Known workspaces", Description: "Current local workspace metadata.", MIMEType: "application/json", Annotations: resourceAnnotations(h.root, 0.8)}, h.readWorkspaces)
	server.AddResource(&sdk.Resource{URI: "prowl://workspace/current/overview", Name: "current-overview", Title: "Current workspace overview", Description: "Structural project overview.", MIMEType: "application/json", Annotations: resourceAnnotations(h.root, 0.8)}, h.readOverview)
	knowledgeRoot := ""
	knowledgeIndex := ""
	if h.knowledge != nil {
		knowledgeRoot = h.knowledge.Root
		knowledgeIndex = filepath.Join(h.knowledge.Root, "index.md")
	}
	server.AddResource(&sdk.Resource{URI: "prowl://workspace/current/knowledge/index", Name: "knowledge-index", Title: "Knowledge index", Description: "Canonical marker-owned OKF knowledge index.", MIMEType: "text/markdown", Annotations: resourceAnnotations(knowledgeIndex, 0.9)}, h.readKnowledgeIndex)
	server.AddResource(&sdk.Resource{URI: "prowl://workspace/current/changes", Name: "current-changes", Title: "Current Git changes", Description: "Project-relative changed and untracked paths.", MIMEType: "application/json", Annotations: resourceAnnotations(h.root, 0.9)}, h.readChanges)
	server.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "prowl://workspace/current/concept/{id}", Name: "concept", Title: "Knowledge concept", Description: "Canonical OKF concept by durable ID.", MIMEType: "text/markdown", Annotations: resourceAnnotations(knowledgeRoot, 0.9)}, h.readConcept)
	server.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "prowl://workspace/current/source/{+path}", Name: "source", Title: "Project source", Description: "Project-relative source file with traversal protection.", MIMEType: "text/plain", Annotations: resourceAnnotations(h.root, 0.8)}, h.readSource)
}

func resourceAnnotations(path string, priority float64) *sdk.Annotations {
	annotations := &sdk.Annotations{Audience: []sdk.Role{sdk.Role("user"), sdk.Role("assistant")}, Priority: priority}
	if path != "" {
		if info, err := os.Stat(path); err == nil {
			annotations.LastModified = info.ModTime().UTC().Format(time.RFC3339)
		}
	}
	return annotations
}

func (h *handlers) readWorkspaces(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	payload := map[string]any{"workspaces": []map[string]any{{"id": "current", "root": h.root}}}
	return textResource(request.Params.URI, "application/json", marshalResource(payload)), nil
}

func (h *handlers) readOverview(ctx context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	if h.beforeCall != nil {
		if err := h.beforeCall(ctx); err != nil {
			return nil, err
		}
	}
	if h.q == nil {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	overview, err := h.q.Overview()
	if err != nil {
		return nil, err
	}
	return textResource(request.Params.URI, "application/json", marshalResource(overview)), nil
}

func (h *handlers) readKnowledgeIndex(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	if h.knowledge == nil {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	data, err := h.knowledge.ReadBundleFile("index.md")
	if os.IsNotExist(err) {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	if err != nil {
		return nil, err
	}
	return textResource(request.Params.URI, "text/markdown", string(data)), nil
}

func (h *handlers) readChanges(ctx context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	files, err := gitChangedFiles(ctx, h.root)
	if err != nil {
		return nil, err
	}
	return textResource(request.Params.URI, "application/json", marshalResource(map[string]any{"base": "HEAD", "files": files})), nil
}

func (h *handlers) readConcept(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	if h.knowledge == nil {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	parsed, err := url.Parse(request.Params.URI)
	if err != nil {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	id, err := url.PathUnescape(strings.TrimPrefix(parsed.EscapedPath(), "/current/concept/"))
	if err != nil || id == "" || len(id) > 1024 {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	documents, err := h.knowledge.List()
	if err != nil {
		return nil, err
	}
	for _, document := range documents {
		documentID := document.Prowl.ID
		if documentID == "" {
			documentID = strings.TrimSuffix(filepath.ToSlash(document.Path), filepath.Ext(document.Path))
		}
		if documentID == id {
			data, err := h.knowledge.Codec.Marshal(document)
			if err != nil {
				return nil, err
			}
			return textResource(request.Params.URI, "text/markdown", string(data)), nil
		}
	}
	return nil, sdk.ResourceNotFoundError(request.Params.URI)
}

func (h *handlers) readSource(_ context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	if h.root == "" {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	parsed, err := url.Parse(request.Params.URI)
	if err != nil {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	escaped := strings.TrimPrefix(parsed.EscapedPath(), "/current/source/")
	relative, err := url.PathUnescape(escaped)
	if err != nil || relative == "" {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	relative = filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	if spec := parsed.Query().Get("lines"); spec != "" {
		start, end, ok := parseLineSpec(spec)
		if !ok {
			return nil, sdk.ResourceNotFoundError(request.Params.URI)
		}
		pk, err := query.PeekLines(h.root, relative, start, end)
		if err != nil {
			return nil, sdk.ResourceNotFoundError(request.Params.URI)
		}
		return textResource(request.Params.URI, "text/plain", pk.Text), nil
	}
	data, err := readRootedSource(h.root, relative)
	if err != nil {
		return nil, sdk.ResourceNotFoundError(request.Params.URI)
	}
	return textResource(request.Params.URI, "text/plain", string(data)), nil
}

// parseLineSpec parses a ?lines= value of "start" or "start-end" into a range.
func parseLineSpec(spec string) (start, end int, ok bool) {
	if i := strings.IndexByte(spec, '-'); i >= 0 {
		s, err1 := strconv.Atoi(strings.TrimSpace(spec[:i]))
		e, err2 := strconv.Atoi(strings.TrimSpace(spec[i+1:]))
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return s, e, true
	}
	s, err := strconv.Atoi(strings.TrimSpace(spec))
	if err != nil {
		return 0, 0, false
	}
	return s, s, true
}

func readRootedSource(rootPath, relative string) ([]byte, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSourceResourceBytes {
		return nil, fmt.Errorf("source resource is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSourceResourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSourceResourceBytes {
		return nil, fmt.Errorf("source resource exceeds %d bytes", maxSourceResourceBytes)
	}
	return data, nil
}

func textResource(uri, mimeType, text string) *sdk.ReadResourceResult {
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: mimeType, Text: text}}}
}

func marshalResource(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func gitChangedFiles(ctx context.Context, root string) ([]string, error) {
	if root == "" {
		return []string{}, nil
	}
	set := map[string]bool{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, arguments := range [][]string{{"diff", "--name-only", "HEAD"}, {"ls-files", "--others", "--exclude-standard"}} {
		command := exec.CommandContext(ctx, "git", arguments...)
		command.Dir = root
		output, err := boundedCommandOutput(command, maxGitOutputBytes)
		if err != nil {
			return nil, fmt.Errorf("git %s failed: %w", strings.Join(arguments, " "), err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if line != "" {
				set[filepath.ToSlash(line)] = true
			}
		}
	}
	files := make([]string, 0, len(set))
	for file := range set {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

func boundedCommandOutput(command *exec.Cmd, limit int64) ([]byte, error) {
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(output)) > limit {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("command output exceeds %d bytes", limit)
	}
	waitErr := command.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}
