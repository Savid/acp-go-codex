package codexacp

import (
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func promptToCodex(blocks []acp.ContentBlock) ([]codex.UserInput, error) {
	input := make([]codex.UserInput, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			input = append(input, codex.UserInput{"type": "text", "text": block.Text.Text})
		case block.Image != nil:
			if block.Image.Uri != nil && *block.Image.Uri != "" {
				if strings.HasPrefix(*block.Image.Uri, "file://") {
					input = append(input, codex.UserInput{"type": "localImage", "path": strings.TrimPrefix(*block.Image.Uri, "file://")})
				} else {
					input = append(input, codex.UserInput{"type": "image", "url": *block.Image.Uri})
				}
			} else {
				input = append(input, codex.UserInput{
					"type": "image",
					"url":  imageDataURL(block.Image.MimeType, block.Image.Data),
				})
			}
		case block.ResourceLink != nil:
			input = append(input, resourceLinkInput(*block.ResourceLink))
		case block.Resource != nil:
			input = append(input, resourceInput(block.Resource.Resource))
		case block.Audio != nil:
			return nil, fmt.Errorf("audio prompt blocks are not supported by Codex app-server")
		}
	}

	return input, nil
}

func imageDataURL(mimeType string, data string) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	return "data:" + mimeType + ";base64," + data
}

func resourceLinkInput(resource acp.ContentBlockResourceLink) codex.UserInput {
	if strings.HasPrefix(resource.Uri, "file://") {
		return codex.UserInput{"type": "mention", "name": firstNonEmpty(resource.Name, resource.Uri), "path": strings.TrimPrefix(resource.Uri, "file://")}
	}

	return codex.UserInput{"type": "text", "text": resource.Uri}
}

func resourceInput(resource acp.EmbeddedResourceResource) codex.UserInput {
	switch {
	case resource.TextResourceContents != nil:
		contents := resource.TextResourceContents
		return codex.UserInput{"type": "text", "text": fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", contents.Uri, contents.Text)}
	case resource.BlobResourceContents != nil:
		contents := resource.BlobResourceContents
		return codex.UserInput{"type": "text", "text": fmt.Sprintf("[resource: %s]", contents.Uri)}
	default:
		return codex.UserInput{"type": "text", "text": "[resource]"}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
