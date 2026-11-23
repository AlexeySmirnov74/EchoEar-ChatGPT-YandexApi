package ai

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Routing is negotiated from the first hello on the same path used by stock
// firmware and the existing gateway. No provider is opened before selection.
func handleHelloRoutedWSWith(w http.ResponseWriter,r *http.Request,legacy,manual func(*websocket.Conn,map[string]any)){
	c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
	c.SetReadLimit(64*1024);_ = c.SetReadDeadline(time.Now().Add(10*time.Second))
	kind,data,err:=c.ReadMessage();if err!=nil||kind!=websocket.TextMessage{return}
	var hello map[string]any
	if json.Unmarshal(data,&hello)!=nil||hello["type"]!="hello"{return}
	_ = c.SetReadDeadline(time.Time{})
	features,_:=hello["features"].(map[string]any)
	if features["persistent_idle"]==true {manual(c,hello);return}
	legacy(c,hello)
}

func serveManagedManualConfigured(c *websocket.Conn,r *http.Request){
	ctx:=r.Context()
	key:=aiString(ctx,"openai_api_key","")
	if key==""{return}
	profile:=deviceProfileFor(ctx,r.Header.Get("Device-Id"))
	model:=override(profile.OpenAIRealtimeModel,aiString(ctx,"openai_realtime_model","gpt-realtime"))
	voice:=override(profile.OpenAITTSVoice,aiString(ctx,"openai_tts_voice","alloy"))
	endpoint,err:=realtimeDialURL(ctx,model);if err!=nil{return}
	prompt:=override(profile.SystemPrompt,globalSystemPrompt(ctx))
	tools:=append(speakerRealtimeTools(), brightnessRealtimeTools()...)
	tools=append(tools,batteryRealtimeTool())
	tools=append(tools,restRealtimeTools()...)
	serveManagedManualConn(c,r,endpoint,key,prompt,voice,tools)
}

func handleManagedManualWithConfig(w http.ResponseWriter,r *http.Request,endpoint,key,prompt,voice string,tools []map[string]any){
	c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
	c.SetReadLimit(64*1024)
	_ = c.SetReadDeadline(time.Now().Add(10*time.Second))
	var hello map[string]any
	if c.ReadJSON(&hello)!=nil{return}
	features,_:=hello["features"].(map[string]any)
	if hello["type"]!="hello"||features["persistent_idle"]!=true{return}
	_ = c.SetReadDeadline(time.Time{})
	serveManagedManualConn(c,withImagePreviewRequest(r,hello),endpoint,key,prompt,voice,tools)
}

// The device uses UTC milliseconds to synchronize its clock and this offset
// for local alarms. Omit untrustworthy dates instead of arming with bad time.
func manualServerTime(now time.Time) map[string]any {
	ms:=now.UnixMilli()
	if ms < 1700000000000 || ms >= 4102358400000 { return nil }
	return map[string]any{"timestamp":ms,"timezone_offset":300}
}

func serveManagedManualConn(c *websocket.Conn,r *http.Request,endpoint,key,prompt,voice string,tools []map[string]any){
	prompt += "\n\nFor a relative alarm request such as 'через две минуты', always call set_alarm_after with the requested number of minutes; never calculate the clock time yourself. Confirm the alarm only after the tool response, using the confirmed time. The device rounds up to the next minute, so the wait can be up to almost one minute longer; explain this briefly. If an alarm creation returns reason unconfirmed_may_have_been_saved, explain that you could not confirm it and it may still have been saved; offer to check the list. Do not automatically retry."
	prompt += "\n\nMention screen brightness control among capabilities only if brightness tools are available. For requests to darken or brighten the screen use the brightness tools, not volume. For a relative change with no specific amount use a 10 percentage point step, clamped to 10-100. Minimum brightness is 10 percent; explain this minimum when asked for less. Wait for the device to accept the command before confirming; acceptance does not confirm the screen has completed its gradual brightness change."
	prompt += "\n\nWhen asked about battery level, call get_battery_estimate if available. Report a percentage only if that tool returns available=true with an estimate; describe it as approximate. If the tool fails or returns available=false, say the measurement is unavailable and never guess a percentage. Charging state is unknown; do not claim that the device is charging or not charging from this tool."
	prompt += "\n\nWhen the alarm tools are available, mention alarms when asked about your capabilities. Set, list or delete alarms only by calling the relevant tool and wait for a successful result before confirming. If a tool reports an error or ok=false, say that the alarm action could not be confirmed; never claim it succeeded. Ask for the missing time or which alarm to delete when ambiguous. Alarms are in the device's Asia/Yekaterinburg local time. Once-only is the default; repeat daily only on request. Alarms cannot sound when the device is switched off or its clock is not synchronized. A sounding alarm continues until the screen is touched; do not claim voice can stop it."
	prompt += "\n\nWhen asked what you can do, describe actual current capabilities briefly: talk, answer questions and tell stories. Mention controlling speaker volume only when the corresponding speaker tool is available. Do not claim access to unprovided tools or device features. You cannot turn off the speaker by voice. Never promise or claim that you can turn it off; if asked to do so, explain briefly that voice power-off is unavailable and suggest holding the device's power button."
	prompt += "\n\nWhen enter_rest is available, requests to sleep, rest, or turn off should call enter_rest with no arguments. Wait for the tool result before speaking: only if success=true and mode=resting briefly say you are going to rest; the screen will turn off and the connection will close after your reply, but local alarms remain active. If the user asked to turn off, explain that this is rest, not power off; full power off requires holding the power button. Do not claim rest started if the result is unsuccessful or unconfirmed, and do not automatically retry an unconfirmed command. When asked about capabilities, mention rest only if the tool is available. A disconnected device wakes locally by touching its screen; do not claim remote wake is possible."
	if enabled, _ := r.Context().Value(imagePreviewContextKey{}).(bool); enabled {
		tools = append(tools, imageRealtimeTool())
		prompt += "\n\nWhen the user asks to see or show a picture or image, you must call the show_image tool with a concise search query in the user's language. When the user asks you to draw, create, or generate a picture, this device cannot draw or generate pictures: instead call show_image to search for an existing picture or drawing matching the current request, and clearly say that you are searching for an existing one. For example, for 'нарисуй оленя', search for 'рисунок оленя' and say 'Я поищу готовый рисунок оленя'. Base every search on the current request; do not reuse a previous subject unless the user asks for it. Do not say that you are searching or will show an image unless you actually call show_image in the same response. If you cannot call the tool, say that you cannot show the image instead of promising to search. The search is asynchronous. Never claim you drew, generated, found, displayed, saw, or inspected an image without confirmation; you do not receive visual confirmation or see the picture."
		prompt += " When asked what you can do, include that you can search the internet for existing pictures and send them to the screen; never claim you generate or draw the pictures."
		if _,ok:=r.Context().Value(imageBackgroundContextKey{}).(imageBackgroundContext);ok {
			tools=append(tools,imageBackgroundTools()...)
			prompt+="\n\nIf the user asks to keep the current picture as the background, call pin_image_background. If the user asks to remove the background or picture, call clear_image_background. Wait for the tool result and only confirm success when the result explicitly says success. If status is uncertain, say that you cannot confirm whether the action was applied; do not claim success or failure. Otherwise explain that it could not be done."
			prompt+=" When asked about capabilities, you can also mention keeping a displayed picture as a background and removing it."
		}
	}
	rt,actorCtx:=newManagedActorRuntime(r.Context(),endpoint,key,prompt,voice,tools);defer rt.Close()
	timer:=time.NewTimer(8*time.Second);defer timer.Stop()
	select{case<-actorCtx.Done():return;case<-timer.C:return;case<-rt.ready:}
	sessionID:=uuid.NewString()
	hello:=map[string]any{"type":"hello","transport":"websocket","session_id":sessionID,"features":map[string]any{"persistent_idle":true},"audio_params":map[string]any{"sample_rate":serverSampleRate,"frame_duration":frameDurationMs}}
	if clock:=manualServerTime(time.Now());clock!=nil { hello["server_time"]=clock }
	if writeManualJSON(c,hello)!=nil{return}
	runExperimentalManualActor(actorCtx,c,rt,sessionID)
}
