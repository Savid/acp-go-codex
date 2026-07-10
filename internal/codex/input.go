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

// ErrImageMissingData reports an image prompt block that carries neither
// inline data nor a URI. Callers map it to the uniform ACP invalid-params
// error for prompt images.
var ErrImageMissingData = errors.New("missing image data or uri")

const (
	inputText       = "text"
	inputURL        = "url"
	inputLocalImage = "localImage"
	inputImage      = "image"
	inputMention    = "mention"
)

// PromptToUserInput maps ACP prompt content blocks into the native Codex
// app-server user-input representation. Mapping fails closed: audio blocks
// return ErrAudioUnsupported, images without data or a URI return
// ErrImageMissingData, and unknown or empty blocks return
// ErrUnsupportedContentBlock.
func PromptToUserInput(blocks []acp.ContentBlock) ([]UserInput, error) {
	input := make([]UserInput, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			input = append(input, UserInput{fieldType: inputText, inputText: block.Text.Text})
		case block.Image != nil:
			imageInput, err := imageUserInput(*block.Image)
			if err != nil {
				return nil, err
			}

			input = append(input, imageInput)
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

	return input, nil
}

func imageUserInput(image acp.ContentBlockImage) (UserInput, error) {
	if image.Uri != nil && *image.Uri != "" {
		if strings.HasPrefix(*image.Uri, "file://") {
			return UserInput{fieldType: inputLocalImage, fieldPath: strings.TrimPrefix(*image.Uri, "file://")}, nil
		}

		return UserInput{fieldType: inputImage, inputURL: *image.Uri}, nil
	}

	if image.Data == "" {
		return nil, ErrImageMissingData
	}

	return UserInput{
		fieldType: inputImage,
		inputURL:  imageDataURL(image.MimeType, image.Data),
	}, nil
}

func imageDataURL(mimeType string, data string) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	return "data:" + mimeType + ";base64," + data
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
		contents := resource.BlobResourceContents

		return UserInput{fieldType: inputText, inputText: fmt.Sprintf("[resource: %s]", contents.Uri)}
	default:
		return UserInput{fieldType: inputText, inputText: "[resource]"}
	}
}
