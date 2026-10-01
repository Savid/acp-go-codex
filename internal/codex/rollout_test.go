package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The ids the gateway returned for the fixture's three responses.
const (
	fixtureFirstResponse  = "gen-1790864938-0aKnVVHJ8cC28luaL5g3"
	fixtureToolResponse   = "gen-1790864945-A9o5yHwj0UtBA1kCYhY4"
	fixtureAnswerResponse = "gen-1790864951-yeQw03kPErLftC8mXx5C"
)

func fixtureRows(t *testing.T) []RolloutRow {
	t.Helper()

	raw, err := ReadRows("testdata/response-ids.jsonl")
	require.NoError(t, err)

	rows := make([]RolloutRow, 0, len(raw))

	for _, row := range raw {
		decoded, decodeErr := DecodeRow(row)
		require.NoError(t, decodeErr)

		rows = append(rows, decoded)
	}

	return rows
}

// ownedRows lists, per response id, the payload types of the rows it owns.
func ownedRows(rows []RolloutRow, ids []string) map[string][]string {
	owned := make(map[string][]string)

	for index, id := range ids {
		if id != "" {
			owned[id] = append(owned[id], stringValue(rows[index].Payload, fieldType))
		}
	}

	return owned
}

// TestResponseIDsFollowTheirRecords proves each response's reasoning and
// assistant rows take the id of the token_usage_record codex wrote when the
// response completed, and no other row takes an id.
func TestResponseIDsFollowTheirRecords(t *testing.T) {
	t.Parallel()

	rows := fixtureRows(t)
	ids := ResponseIDs(rows)

	require.Len(t, ids, len(rows))
	require.Equal(t, map[string][]string{
		fixtureFirstResponse:  {itemReasoning, itemMessage},
		fixtureToolResponse:   {itemReasoning},
		fixtureAnswerResponse: {itemReasoning, itemMessage},
	}, ownedRows(rows, ids))
}

// TestResponseIDsLeaveUnrecordedOutputUnnamed proves an output row no record
// of its turn claims carries no id: a response that failed before its turn
// ended, and output a record of another turn follows.
func TestResponseIDsLeaveUnrecordedOutputUnnamed(t *testing.T) {
	t.Parallel()

	rows := fixtureRows(t)

	var reasoning, record RolloutRow

	for _, row := range rows {
		switch {
		case row.Type == RowTypeResponseItem && stringValue(row.Payload, fieldType) == itemReasoning:
			reasoning = row
		case row.Type == RowTypeUsageRecord:
			record = row
		}
	}

	started := RolloutRow{Type: RowTypeEventMsg, Payload: map[string]any{fieldType: eventTaskStarted, "turn_id": "failed-turn"}}
	completed := RolloutRow{Type: RowTypeEventMsg, Payload: map[string]any{fieldType: eventTaskComplete, "turn_id": "failed-turn"}}

	failed := append(append([]RolloutRow{}, rows...), started, reasoning, completed)
	require.Empty(t, ResponseIDs(failed)[len(rows)+1], "a failed response's output has no record")

	foreign := append(append([]RolloutRow{}, rows...), started, reasoning, record)
	require.Empty(t, ResponseIDs(foreign)[len(rows)+1], "a record of another turn claims nothing")
}
