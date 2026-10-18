package httpapi

import "testing"

func TestAndroidPushTokensAreOpaqueAndDoNotUseAPNsValidation(t *testing.T) {
	cases := []struct {
		device pushDeviceRequest
		valid  bool
	}{
		{pushDeviceRequest{Platform: "android", FCMToken: "opaque:FCM-Token_abc"}, true},
		{pushDeviceRequest{Platform: "android", FCMToken: ""}, false},
		{pushDeviceRequest{Platform: "android", FCMToken: "token", AlertToken: "aa"}, false},
		{pushDeviceRequest{Platform: "ios", FCMToken: "token"}, false},
		{pushDeviceRequest{AlertToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, true},
		{pushDeviceRequest{Platform: "unknown", FCMToken: "token"}, false},
		{pushDeviceRequest{Platform: "android", FCMToken: "token\nwith-control"}, false},
	}
	for i, c := range cases {
		if validPushTokens(c.device) != c.valid {
			t.Errorf("case %d", i)
		}
	}
}
