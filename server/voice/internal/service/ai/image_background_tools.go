package ai

func imageBackgroundTools() []map[string]any {
	return []map[string]any{
		{"type":"function","name":"pin_image_background","description":"Keep the currently displayed image as the speaker's background when the user explicitly asks to keep, pin or use it as a background. Await the tool result before reporting success.","parameters":map[string]any{"type":"object","properties":map[string]any{},"additionalProperties":false}},
		{"type":"function","name":"clear_image_background","description":"Clear the current image and pinned background and restore the robot face when the user asks. Await the tool result before reporting success.","parameters":map[string]any{"type":"object","properties":map[string]any{},"additionalProperties":false}},
	}
}
