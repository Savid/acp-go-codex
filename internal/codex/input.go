package codex

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// ErrAudioUnsupported reports that a prompt carried an audio content block,
// which the Codex app-server cannot accept. Callers map it to the uniform ACP
// unsupported-content error.
var ErrAudioUnsupported = errors.New("audio prompt blocks are not supported by Codex app-server")

// ErrUnsupportedContentBlock reports a prompt content block that carries no
// recognized content. Callers map it to the uniform ACP unsupported-content
// error; nothing is silently dropped.
var ErrUnsupportedContentBlock = errors.New("unsupported prompt content block")

// ErrImageNotMaterialized reports that the caller supplied fewer or more
// materialized images than the prompt carries image-bearing blocks. Every
// image must be validated and materialized before native mapping.
var ErrImageNotMaterialized = errors.New("prompt image count does not match materialized images")

const (
	inputText       = "text"
	inputURL        = "url"
	inputLocalImage = "localImage"
	inputImage      = "image"
	inputMention    = "mention"
)

// PromptImage is one validated prompt image in its native transport form:
// a wrapper-owned local file path, or a data URL when no local file exists.
type PromptImage struct {
	LocalPath string
	DataURL   string
}

// IsImageBearingBlock reports whether a prompt content block maps to native
// Codex image input: a standard image block, or an embedded blob resource
// declaring an image media type.
func IsImageBearingBlock(block acp.ContentBlock) bool {
	if block.Image != nil {
		return true
	}

	if block.Resource == nil || block.Resource.Resource.BlobResourceContents == nil {
		return false
	}

	mimeType := block.Resource.Resource.BlobResourceContents.MimeType

	return mimeType != nil && strings.HasPrefix(strings.ToLower(*mimeType), "image/")
}

// PromptToUserInput maps ACP prompt content blocks into the native Codex
// app-server user-input representation. Image-bearing blocks consume the
// caller-validated materialized images in request order; embedded data is
// authoritative and block URIs are provenance only, never followed. Mapping
// fails closed: audio blocks return ErrAudioUnsupported, unknown or empty
// blocks return ErrUnsupportedContentBlock, and an image count mismatch
// returns ErrImageNotMaterialized.
func PromptToUserInput(blocks []acp.ContentBlock, images []PromptImage) ([]UserInput, error) {
	input := make([]UserInput, 0, len(blocks))
	nextImage := 0

	for _, block := range blocks {
		if IsImageBearingBlock(block) {
			if nextImage >= len(images) {
				return nil, ErrImageNotMaterialized
			}

			imageInput, err := imageUserInput(images[nextImage])
			if err != nil {
				return nil, err
			}

			nextImage++

			input = append(input, imageInput)

			continue
		}

		switch {
		case block.Text != nil:
			input = append(input, UserInput{fieldType: inputText, inputText: block.Text.Text})
		case block.ResourceLink != nil:
			input = append(input, resourceLinkInput(*block.ResourceLink))
		case block.Resource != nil:
			input = append(input, resourceInput(block.Resource.Resource))
		case block.Audio != nil:
			return nil, ErrAudioUnsupported
		default:
			return nil, ErrUnsupportedContentBlock
		}
	}

	if nextImage != len(images) {
		return nil, ErrImageNotMaterialized
	}

	return input, nil
}

func imageUserInput(image PromptImage) (UserInput, error) {
	if image.LocalPath != "" {
		return UserInput{fieldType: inputLocalImage, fieldPath: image.LocalPath}, nil
	}

	if image.DataURL != "" {
		return UserInput{fieldType: inputImage, inputURL: image.DataURL}, nil
	}

	return nil, ErrImageNotMaterialized
}

func resourceLinkInput(resource acp.ContentBlockResourceLink) UserInput {
	if strings.HasPrefix(resource.Uri, "file://") {
		return UserInput{fieldType: inputMention, fieldName: firstNonEmpty(resource.Name, resource.Uri), fieldPath: strings.TrimPrefix(resource.Uri, "file://")}
	}

	return UserInput{fieldType: inputText, inputText: resource.Uri}
}

func resourceInput(resource acp.EmbeddedResourceResource) UserInput {
	switch {
	case resource.TextResourceContents != nil:
		contents := resource.TextResourceContents

		return UserInput{fieldType: inputText, inputText: fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", contents.Uri, contents.Text)}
	case resource.BlobResourceContents != nil:
		return UserInput{fieldType: inputText, inputText: fmt.Sprintf("[resource: %s]", resource.BlobResourceContents.Uri)}
	default:
		return UserInput{fieldType: inputText, inputText: "[resource]"}
	}
}
