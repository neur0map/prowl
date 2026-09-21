package openai

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartRequestsCodexConnectorScopes(t *testing.T) {
	flow, err := Start(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })

	authorize, err := url.Parse(flow.URL)
	require.NoError(t, err)
	scopes := map[string]bool{}
	for _, scope := range strings.Fields(authorize.Query().Get("scope")) {
		scopes[scope] = true
	}
	require.True(t, scopes["api.connectors.read"])
	require.True(t, scopes["api.connectors.invoke"])
}
