package codexacp

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/wire"
)

// appendStoredRow republishes a session generation with one extra native row,
// standing in for content a store already holds that the adapter must refuse.
func appendStoredRow(t *testing.T, store acpcore.SessionStore, sessionID acp.SessionId, row string) {
	t.Helper()

	generation, err := store.Load(t.Context(), string(sessionID))
	require.NoError(t, err)

	main := acpcore.SessionKey{SessionID: string(sessionID)}
	replacements := []acpcore.SessionStoreReplacement{
		{Key: main, Entries: append(generation[acpcore.SessionStoreMainSubpath], acpcore.SessionStoreEntry(row))},
	}

	for subpath, entries := range generation {
		if subpath != acpcore.SessionStoreMainSubpath {
			replacements = append(replacements, acpcore.SessionStoreReplacement{
				Key: acpcore.SessionKey{SessionID: string(sessionID), Subpath: subpath}, Entries: entries,
			})
		}
	}

	require.NoError(t, store.Replace(t.Context(), main, replacements))
}

// Structurally malformed rows are refused on every restore route, because
// hydrate decodes them before they become native state. A row whose image
// bytes are corrupt is a projection verdict, so only load reaches it.
func TestRestoreRejectsMalformedNativeRow(t *testing.T) {
	cases := map[string]struct {
		row    string
		routes []bool
	}{
		"malformed_type": {row: `{"type":7}`, routes: []bool{true, false}},
		"corrupt_image": {
			row:    `{"type":"response_item","payload":{"type":"image_generation_call","id":"generated-image","status":"completed","result":"!!!"}}`,
			routes: []bool{true},
		},
	}

	for name, tc := range cases {
		for _, replay := range tc.routes {
			route := "resume"
			if replay {
				route = "load"
			}

			row := tc.row

			t.Run(name+"/"+route, func(t *testing.T) {
				store := acpcore.NewInMemorySessionStore()
				h := newHarness(t, WithSessionStore(store))
				h.initialize()
				cwd := t.TempDir()
				created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
				require.NoError(t, err)
				_, err = h.prompt(created.SessionId, "HELLO", nil)
				require.NoError(t, err)
				_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
				require.NoError(t, err)
				appendStoredRow(t, store, created.SessionId, row)

				if replay {
					_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
				} else {
					_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
				}

				var refused *acp.RequestError
				require.ErrorAs(t, err, &refused, "restore succeeded after silently discarding or forwarding corrupt stored content")
				require.Equal(t, vendor+"_restore_failed", requestErrorData(t, err)["error"])
			})
		}
	}
}

// A newer native file must pass validation before it can replace the mirror.
func TestRestoreRejectsMalformedNativeExtension(t *testing.T) {
	for _, replay := range []bool{true, false} {
		route := "resume"
		if replay {
			route = "load"
		}

		t.Run(route, func(t *testing.T) {
			store := acpcore.NewInMemorySessionStore()
			h := newHarness(t, WithSessionStore(store))
			h.initialize()
			cwd := t.TempDir()
			created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
			require.NoError(t, err)
			_, err = h.prompt(created.SessionId, "HELLO", nil)
			require.NoError(t, err)
			_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
			require.NoError(t, err)
			before, err := store.Load(h.ctx(), string(created.SessionId))
			require.NoError(t, err)
			var record sessionRecord
			require.NoError(t, json.Unmarshal(before["config"][0], &record))
			file, err := os.OpenFile(record.RolloutPath, os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = file.WriteString(`{"type":7}` + "\n")
			require.NoError(t, err)
			require.NoError(t, file.Close())

			if replay {
				_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
			} else {
				_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
			}

			require.Error(t, err)
			require.Equal(t, vendor+"_restore_failed", requestErrorData(t, err)["error"])
			after, err := store.Load(h.ctx(), string(created.SessionId))
			require.NoError(t, err)
			require.Equal(t, before, after, "malformed native rows must not replace the valid mirror")
		})
	}
}
