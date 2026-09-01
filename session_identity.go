package codexacp

import (
	"context"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	codexTurnIDMetaKey    = "turnId"
	codexMessageIDMetaKey = "messageId"
)

// nativeTurnIdentity is the provider-owned correlation for one Codex turn and
// its terminal assistant response item. Live values originate only in the
// native app-server event source; replayed values may originate in the durable
// rollout the adapter imported earlier.
type nativeTurnIdentity struct {
	turnID    string
	messageID string
}

func nativeIdentityFromEvent(event codex.Event, prior nativeTurnIdentity) nativeTurnIdentity {
	identity := prior
	if event.TurnID != "" {
		identity.turnID = event.TurnID
	}

	if event.Kind == codex.EventAgentMessageDelta && event.ItemID != "" {
		identity.messageID = event.ItemID
	}

	return identity
}

func nativeIdentityValues(identity nativeTurnIdentity) map[string]any {
	values := make(map[string]any, 2)
	if identity.turnID != "" {
		values[codexTurnIDMetaKey] = identity.turnID
	}

	if identity.messageID != "" {
		values[codexMessageIDMetaKey] = identity.messageID
	}

	return values
}

func nativeIdentityNotificationMeta(ctx context.Context, identity nativeTurnIdentity) map[string]any {
	return mergeNativeIdentityMeta(turnRouteMetaFromContext(ctx), identity)
}

func mergePromptResponseMeta(meta map[string]any, identity nativeTurnIdentity) map[string]any {
	return mergeNativeIdentityMeta(meta, identity)
}

func mergeNativeIdentityMeta(meta map[string]any, identity nativeTurnIdentity) map[string]any {
	identityValues := nativeIdentityValues(identity)
	if len(identityValues) == 0 {
		return meta
	}

	meta = cloneAnyMap(meta)
	if meta == nil {
		meta = make(map[string]any, 1)
	}

	codexMeta, _ := meta[codexMetaKey].(map[string]any)

	codexMeta = cloneAnyMap(codexMeta)
	if codexMeta == nil {
		codexMeta = make(map[string]any, len(identityValues))
	}

	for key, value := range identityValues {
		codexMeta[key] = value
	}

	meta[codexMetaKey] = codexMeta

	return meta
}

func (s *session) emitUpdatesWithNativeIdentity(
	ctx context.Context,
	identity nativeTurnIdentity,
	updates ...acp.SessionUpdate,
) error {
	if len(updates) > 0 {
		s.agent.observe.ObserveFirstPromptUpdate(ctx)
	}

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	for _, update := range updates {
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			Meta:      nativeIdentityNotificationMeta(ctx, identity),
			SessionId: s.id,
			Update:    update,
		}); err != nil {
			return wrapHostDeliveryError(err)
		}
	}

	return nil
}

func nativeIdentityChanged(left, right nativeTurnIdentity) bool {
	return left.turnID != right.turnID || left.messageID != right.messageID
}

// finalizePromptNativeIdentity publishes the exact terminal identity observed
// on the native app-server stream. The rollout is durability, never a second
// live event or identity source.
func (s *session) finalizePromptNativeIdentity(ctx context.Context, state *promptEventState) error {
	if !nativeIdentityChanged(state.nativeIdentity, state.emittedNativeIdentity) {
		return nil
	}

	if err := s.emitUpdatesWithNativeIdentity(
		ctx,
		state.nativeIdentity,
		acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	); err != nil {
		return err
	}

	state.emittedNativeIdentity = state.nativeIdentity

	return nil
}

func rolloutNativeTerminalIdentity(entries []SessionStoreEntry) nativeTurnIdentity {
	var identity nativeTurnIdentity

	for _, entry := range entries {
		row, err := decodeRolloutRow(entry)
		if err != nil {
			continue
		}

		turnID := rolloutIdentityString(row.Payload, codexTurnIDMetaKey, "turn_id", "turnID")
		if turnID != "" {
			if identity.turnID != "" && turnID != identity.turnID {
				identity = nativeTurnIdentity{}
			}

			identity.turnID = turnID
		}

		if rolloutAssistantPayload(row.Payload) {
			if messageID := rolloutIdentityString(row.Payload, "id", "itemId", codexMessageIDMetaKey, "uuid"); messageID != "" {
				identity.messageID = messageID
			}
		}
	}

	return identity
}

func rolloutAssistantPayload(payload map[string]any) bool {
	typeName := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		stringFromAny(payload[jsonFieldType]),
		stringFromAny(payload["kind"]),
	)))
	role := strings.ToLower(strings.TrimSpace(stringFromAny(payload["role"])))

	return typeName == valueAgentMessage || typeName == "agentmessage" || typeName == roleAssistant ||
		(typeName == jsonFieldMessage && role == roleAssistant) || role == roleAssistant
}

func rolloutIdentityString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringFromAny(values[key])); value != "" {
			return value
		}
	}

	return ""
}
