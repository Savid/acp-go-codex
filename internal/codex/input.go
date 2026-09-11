package codex

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// ErrUnsupportedContentBlock reports a prompt content block Codex cannot
// accept. Nothing is silently dropped.
var ErrUnsupportedContentBlock = fmt.Errorf("unsupported prompt content block")

const (
	inputText    = "text"
	inputImage   = "image"
	inputMention = "mention"
	inputURL     = "url"

	// fileURIPrefix is the scheme a host sends a local file under. Only the
	// empty authority maps to a host path.
	fileURIPrefix = "file://"
)

// UserInput is one element of a turn/start input array.
type UserInput map[string]any

// PromptImage is one validated prompt image: its raw bytes and sniffed MIME.
type PromptImage struct {
	Data []byte
	MIME string
}

// PromptToUserInput maps ACP prompt blocks to the app-server's input shape.
// Image-bearing blocks consume the validated images in request order and
// travel as data URLs; embedded data is authoritative and block URIs are
// provenance only.
func PromptToUserInput(blocks []acp.ContentBlock, images []PromptImage) ([]UserInput, error) {
	input := make([]UserInput, 0, len(blocks))
	nextImage := 0

	for _, block := range blocks {
		switch {
		case block.Image != nil:
			if nextImage >= len(images) {
				return nil, ErrUnsupportedContentBlock
			}

			image := images[nextImage]
			nextImage++

			input = append(input, UserInput{fieldType: inputImage, inputURL: "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Data)})
		case block.Text != nil:
			input = append(input, UserInput{fieldType: inputText, inputText: block.Text.Text})
		case block.ResourceLink != nil:
			input = append(input, resourceLinkInput(*block.ResourceLink))
		case block.Resource != nil:
			if text := block.Resource.Resource.TextResourceContents; text != nil {
				input = append(input, UserInput{fieldType: inputText, inputText: fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", text.Uri, text.Text)})

				continue
			}

			return nil, ErrUnsupportedContentBlock
		default:
			return nil, ErrUnsupportedContentBlock
		}
	}

	if nextImage != len(images) {
		return nil, ErrUnsupportedContentBlock
	}

	return input, nil
}

func resourceLinkInput(resource acp.ContentBlockResourceLink) UserInput {
	if path, ok := strings.CutPrefix(resource.Uri, fileURIPrefix); ok && strings.HasPrefix(path, "/") {
		return UserInput{fieldType: inputMention, fieldName: firstNonEmpty(resource.Name, resource.Uri), fieldPath: path}
	}

	return UserInput{fieldType: inputText, inputText: resource.Uri}
}
