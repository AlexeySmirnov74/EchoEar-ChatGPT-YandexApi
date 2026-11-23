package ai

import (
	"net/http/httptest"
	"testing"
)

func TestBackgroundCapabilityRequiresBothFeaturesAndIdentity(t *testing.T) {
	for _,tc:=range []struct{name string;preview,background bool;device string;want bool}{
		{"old device",true,false,"device",false},{"no preview",false,true,"device",false},
		{"missing identity",true,true,"",false},{"supported",true,true,"device",true},
	} {t.Run(tc.name,func(t *testing.T){
		r:=httptest.NewRequest("GET","/",nil);r.Header.Set("Device-Id",tc.device)
		r=withImagePreviewRequest(r,map[string]any{"features":map[string]any{"image_preview":tc.preview,"image_background_v1":tc.background}})
		_,got:=r.Context().Value(imageBackgroundContextKey{}).(imageBackgroundContext);if got!=tc.want{t.Fatalf("background capability=%v",got)}
	})}
}
