package server

import (
	"bytes"
	"reflect"
	"testing"
)

func TestWatchUserSerializeExactFrame(t *testing.T) {
	got, err := (&WatchUser{}).Serialize(&WatchUser{Username: "alice"})
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	var payload bytes.Buffer
	putWireUint32(&payload, uint32(CodeWatchUser))
	putWireString(&payload, "alice")
	want := packServerWire(&payload)
	if !bytes.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}

func TestWatchUserDeserializePresenceStates(t *testing.T) {
	tests := []struct {
		name    string
		exists  bool
		status  UserStatus
		country string
	}{
		{name: "not found", exists: false},
		{name: "offline", exists: true, status: StatusOffline},
		{name: "away", exists: true, status: StatusAway, country: "SE"},
		{name: "online", exists: true, status: StatusOnline, country: "US"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload bytes.Buffer
			putWireUint32(&payload, uint32(CodeWatchUser))
			putWireString(&payload, "alice")
			if tt.exists {
				_ = payload.WriteByte(1)
				for _, value := range []uint32{uint32(tt.status), 1200, 3, 7, 10, 2} {
					putWireUint32(&payload, value)
				}
				if tt.status == StatusAway || tt.status == StatusOnline {
					putWireString(&payload, tt.country)
				}
			} else {
				_ = payload.WriteByte(0)
			}

			var got WatchUser
			if err := got.Deserialize(bytes.NewReader(packServerWire(&payload))); err != nil {
				t.Fatalf("Deserialize: %v", err)
			}
			want := WatchUser{Username: "alice", Exists: tt.exists, Status: tt.status, CountryCode: tt.country}
			if tt.exists {
				want.AverageSpeed, want.UploadNumber, want.Unknown, want.Files, want.Directories = 1200, 3, 7, 10, 2
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded = %+v, want %+v", got, want)
			}
		})
	}
}
