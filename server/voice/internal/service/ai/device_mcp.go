package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// deviceMCPBridge correlates only replies to requests made on this connection.
// It never claims success until the device has acknowledged the command.
type deviceMCPBridge struct {
	mu        sync.Mutex
	nextID    int
	pending   map[int]chan map[string]any
	send      func(map[string]any) error
	sessionID string
}

func newDeviceMCPBridge(send func(map[string]any) error, sessionID string) *deviceMCPBridge {
	return &deviceMCPBridge{send: send, sessionID: sessionID, pending: make(map[int]chan map[string]any)}
}

func isSpeakerTool(name string) bool {
	return name == "get_speaker_volume" || name == "set_speaker_volume" || name == "set_alarm" || name == "set_alarm_after" || name == "list_alarms" || name == "delete_alarm" || name == "set_alarm_volume_floor" || name == "enter_rest" || name == "get_rest_status" || name == "wake_from_rest"
}

func restRealtimeTools() []map[string]any {
	empty := func() map[string]any { return map[string]any{"type":"object", "properties":map[string]any{}, "additionalProperties":false} }
	return []map[string]any{
		{"type":"function", "name":"enter_rest", "description":"Ask the speaker to enter rest mode. Call before confirming, then speak a brief confirmation only if success=true and mode=resting. This turns off the screen and disconnects after the confirmation, but is not hardware power off; local alarms remain active. If the tool fails or is unconfirmed, say you cannot confirm and do not retry automatically.", "parameters":empty()},
		{"type":"function", "name":"get_rest_status", "description":"Read the speaker's rest state while connected.", "parameters":empty()},
		{"type":"function", "name":"wake_from_rest", "description":"Cancel rest if still connected. A disconnected speaker cannot be awakened remotely; touch its screen instead.", "parameters":empty()},
	}
}

func speakerRealtimeTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "name": "get_speaker_volume", "description": "Read this speaker's current output volume. Use before increasing or decreasing it by a relative amount.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		{"type": "function", "name": "set_speaker_volume", "description": "Set this speaker's output volume to an exact percentage from 0 through 100. Read the current volume first for a relative change. Only say it was changed after the tool succeeds.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"volume": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}}, "required": []string{"volume"}, "additionalProperties": false}},
		{"type":"function", "name":"set_alarm", "description":"Create an alarm on the speaker in its local timezone Asia/Yekaterinburg. One-time by default; daily only if explicitly requested. day=0 next occurrence, 1 tomorrow, 2 today. Ask a clarifying question for unspecified hour or minute and for dates other than today/tomorrow. Confirm only after a successful result; alarms cannot ring when power is off or the clock is unsynchronized after boot.", "parameters":map[string]any{"type":"object", "properties":map[string]any{"hour":map[string]any{"type":"integer","minimum":0,"maximum":23},"minute":map[string]any{"type":"integer","minimum":0,"maximum":59},"day":map[string]any{"type":"integer","minimum":0,"maximum":2},"daily":map[string]any{"type":"boolean"}}, "required":[]string{"hour","minute","day","daily"},"additionalProperties":false}},
		{"type":"function", "name":"set_alarm_after", "description":"Set a one-time alarm after the requested number of minutes, from 1 through 1439. Use for relative requests such as in two minutes or in one hour; never calculate a wall time yourself. The device rounds up to the next minute, so the wait can be almost a minute longer. Report the exact local time confirmed by the device. If creation cannot be confirmed, warn it may still have been saved and offer to check the alarm list. Do not automatically retry.", "parameters":map[string]any{"type":"object","properties":map[string]any{"minutes":map[string]any{"type":"integer","minimum":1,"maximum":1439}},"required":[]string{"minutes"},"additionalProperties":false}},
		{"type":"function", "name":"list_alarms", "description":"List saved local alarms before answering questions or choosing which alarm to delete. next_local is local wall time in Asia/Yekaterinburg.", "parameters":map[string]any{"type":"object","properties":map[string]any{},"additionalProperties":false}},
		{"type":"function", "name":"delete_alarm", "description":"Delete an alarm by id after listing alarms; ask which one if ambiguous. Confirm only after success.", "parameters":map[string]any{"type":"object","properties":map[string]any{"id":map[string]any{"type":"integer","minimum":1,"maximum":2147483647}},"required":[]string{"id"},"additionalProperties":false}},
		{"type":"function", "name":"set_alarm_volume_floor", "description":"Set the minimum alarm volume percentage. Zero uses current speaker volume. The normal speaker volume is unaffected.","parameters":map[string]any{"type":"object","properties":map[string]any{"floor":map[string]any{"type":"integer","minimum":0,"maximum":100}},"required":[]string{"floor"},"additionalProperties":false}},
	}
}

func brightnessRealtimeTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "name": "get_screen_brightness", "description": "Read the screen brightness percentage. Call first for relative changes.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
		{"type": "function", "name": "set_screen_brightness", "description": "Set the screen brightness in percent from 10 to 100. The minimum 10 prevents a black screen. For a relative change read the current value first and, unless the user requests a different amount, change it by exactly 10 percentage points, clamped to 10-100. The screen fades to the new brightness after acceptance; tool success means the device accepted the command, not that the transition is complete. Tell the user if a requested lower value was limited to 10.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"brightness": map[string]any{"type": "integer", "minimum": 10, "maximum": 100}}, "required": []string{"brightness"}, "additionalProperties": false}},
	}
}

func batteryRealtimeTool() map[string]any {
	return map[string]any{"type": "function", "name": "get_battery_estimate", "description": "Request this device's experimental battery estimate. The estimated percentage is approximate and uncalibrated, may be unavailable, and does not indicate charging. Never guess a percentage or charging state if the measurement is unavailable or fails.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}
}

func (b *deviceMCPBridge) call(ctx context.Context, name string, args map[string]any) (string, error) {
	var tool string
	arguments := map[string]any{}
	switch name {
	case "enter_rest", "get_rest_status", "wake_from_rest":
		if len(args) != 0 { return "", errors.New("unexpected arguments") }
		switch name { case "enter_rest": tool = "self.rest.enter"; case "get_rest_status": tool = "self.rest.status"; case "wake_from_rest": tool = "self.rest.wake" }
	case "get_speaker_volume", "get_screen_brightness":
		if len(args) != 0 {
			return "", errors.New("unexpected arguments")
		}
		tool = "self.get_device_status"
	case "get_battery_estimate":
		if len(args) != 0 {
			return "", errors.New("unexpected arguments")
		}
		tool = "self.battery.get_estimate"
	case "set_speaker_volume":
		v, ok := args["volume"].(float64)
		if !ok || len(args) != 1 || math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v < 0 || v > 100 {
			return "", errors.New("volume must be an integer between 0 and 100")
		}
		tool = "self.audio_speaker.set_volume"
		arguments["volume"] = int(v)
	case "set_screen_brightness":
		v, ok := args["brightness"].(float64)
		if !ok || len(args) != 1 || math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v < 10 || v > 100 {
			return "", errors.New("brightness must be an integer between 10 and 100")
		}
		tool = "self.screen.set_brightness"
		arguments["brightness"] = int(v)
	case "list_alarms":
		if len(args) != 0 { return "", errors.New("unexpected arguments") }
		tool = "self.alarm.list"
	case "set_alarm":
		if len(args) != 4 { return "", errors.New("invalid alarm arguments") }
		for _, spec := range []struct{name string; max float64}{{"hour",23},{"minute",59},{"day",2}} {
			v, ok := args[spec.name].(float64)
			if !ok || math.IsNaN(v) || math.IsInf(v,0) || math.Trunc(v) != v || v < 0 || v > spec.max { return "", errors.New("invalid alarm time") }
			arguments[spec.name] = int(v)
		}
		daily, ok := args["daily"].(bool); if !ok { return "", errors.New("invalid alarm repeat") }
		arguments["daily"] = daily
		tool = "self.alarm.set"
	case "set_alarm_after":
		v, ok := args["minutes"].(float64)
		if !ok || len(args) != 1 || math.IsNaN(v) || math.IsInf(v,0) || math.Trunc(v)!=v || v<1 || v>1439 { return "", errors.New("relative alarm minutes must be an integer from 1 to 1439") }
		arguments["minutes"] = int(v)
		tool = "self.alarm.set_after"
	case "delete_alarm", "set_alarm_volume_floor":
		key, min, max := "id", float64(1), float64(math.MaxInt32)
		tool = "self.alarm.delete"
		if name == "set_alarm_volume_floor" { key, min, max, tool = "floor", 0, 100, "self.alarm.volume_floor" }
		v, ok := args[key].(float64)
		if !ok || len(args) != 1 || math.IsNaN(v) || math.IsInf(v,0) || math.Trunc(v)!=v || v<min || v>max { return "", errors.New("invalid alarm argument") }
		arguments[key] = int(v)
	default:
		return "", errors.New("unsupported device tool")
	}

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan map[string]any, 1)
	b.pending[id] = ch
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, id); b.mu.Unlock() }()
	message := map[string]any{"type": "mcp", "session_id": b.sessionID, "payload": map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments},
	}}
	if err := b.send(message); err != nil {
		return "", fmt.Errorf("device request failed: %w", err)
	}
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", errors.New("device did not confirm the command")
	case reply := <-ch:
		if e, ok := reply["error"]; ok {
			return "", fmt.Errorf("device rejected command: %v", e)
		}
		result, ok := reply["result"].(map[string]any)
		if !ok || result["isError"] == true {
			return "", errors.New("device reported command failure or invalid result")
		}
		content, ok := result["content"].([]any)
		if !ok || len(content) != 1 {
			return "", errors.New("invalid device response")
		}
		item, ok := content[0].(map[string]any)
		if !ok || item["type"] != "text" {
			return "", errors.New("invalid device response")
		}
		value, ok := item["text"].(string)
		if !ok {
			return "", errors.New("invalid device response")
		}
		if name == "enter_rest" || name == "get_rest_status" || name == "wake_from_rest" {
			var status struct { Success *bool `json:"success"`; Mode string `json:"mode"` }
			if json.Unmarshal([]byte(value), &status) != nil || status.Success == nil || (status.Mode != "resting" && status.Mode != "awake") {
				return "", errors.New("invalid rest device response")
			}
			if *status.Success && ((name == "enter_rest" && status.Mode != "resting") || (name == "wake_from_rest" && status.Mode != "awake")) {
				return "", errors.New("contradictory rest device response")
			}
			out, _ := json.Marshal(map[string]any{"success": *status.Success, "mode": status.Mode})
			return string(out), nil
		}
		if name == "set_alarm_volume_floor" {
			if value != "true" { return "", errors.New("device did not confirm alarm volume") }
			return `{"success":true}`, nil
		}
		if name == "set_alarm_after" {
			var confirmation struct { Daily *bool `json:"daily"` }
			if json.Unmarshal([]byte(value), &confirmation) != nil || confirmation.Daily == nil || *confirmation.Daily {
				return "", errors.New("device did not confirm a one-time alarm")
			}
			return translateAlarmReply("set_alarm", value)
		}
		if name == "set_alarm" || name == "list_alarms" || name == "delete_alarm" { return translateAlarmReply(name, value) }
		if name == "set_speaker_volume" || name == "set_screen_brightness" {
			if value != "true" {
				return "", errors.New("device did not confirm the change")
			}
			return `{"success":true}`, nil
		}
		if name == "get_screen_brightness" {
			var status struct { Screen struct { Brightness *int `json:"brightness"` } `json:"screen"` }
			if json.Unmarshal([]byte(value), &status) != nil || status.Screen.Brightness == nil || *status.Screen.Brightness < 0 || *status.Screen.Brightness > 100 {
				return "", errors.New("device did not return a valid screen brightness")
			}
			out, _ := json.Marshal(map[string]int{"brightness": *status.Screen.Brightness})
			return string(out), nil
		}
		if name == "get_battery_estimate" {
			var status struct {
				Available *bool `json:"available"`
				Percent *int `json:"estimated_percent"`
				Approximate *bool `json:"approximate"`
				ChargingStatus string `json:"charging_status"`
			}
			if json.Unmarshal([]byte(value), &status) != nil || status.Available == nil {
				return "", errors.New("device did not return a valid battery status")
			}
			if !*status.Available { return `{"available":false}`, nil }
			if status.Percent == nil || *status.Percent < 0 || *status.Percent > 100 || status.Approximate == nil || !*status.Approximate || status.ChargingStatus != "unknown" {
				return "", errors.New("device did not return a valid approximate battery estimate")
			}
			out, _ := json.Marshal(map[string]any{"available": true, "estimated_percent": *status.Percent, "approximate": true, "charging_status": "unknown"})
			return string(out), nil
		}
		var status struct {
			AudioSpeaker struct {
				Volume *int `json:"volume"`
			} `json:"audio_speaker"`
		}
		if json.Unmarshal([]byte(value), &status) != nil || status.AudioSpeaker.Volume == nil || *status.AudioSpeaker.Volume < 0 || *status.AudioSpeaker.Volume > 100 {
			return "", errors.New("device did not return a valid volume")
		}
		out, _ := json.Marshal(map[string]int{"volume": *status.AudioSpeaker.Volume})
		return string(out), nil
	}
}

func (b *deviceMCPBridge) handleReply(message map[string]any) {
	payload, ok := message["payload"].(map[string]any)
	if !ok || payload["jsonrpc"] != "2.0" {
		return
	}
	id, ok := payload["id"].(float64)
	if !ok || id != math.Trunc(id) || id < 1 || id > float64(math.MaxInt32) {
		return
	}
	b.mu.Lock()
	ch := b.pending[int(id)]
	if ch != nil {
		select {
		case ch <- payload:
		default:
		}
	}
	b.mu.Unlock()
}
