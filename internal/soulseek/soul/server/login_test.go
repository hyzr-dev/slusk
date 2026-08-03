package server

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/internal"
)

func TestLoginSerializeGoldenBytes(t *testing.T) {
	l := &Login{}
	got, err := l.Serialize(&Login{Username: "testuser", Password: "testpass"})
	if err != nil {
		t.Fatalf("Serialize: unexpected error: %v", err)
	}

	// Golden bytes captured for Username="testuser", Password="testpass".
	// Layout: size(4) | code(4)=1 | username(4+8) | password(4+8) |
	// versionMajor(4)=160 | md5sum-as-string(4+32) | versionMinor(4)=1.
	want := []byte{
		0x48, 0x0, 0x0, 0x0, // size = 72
		0x1, 0x0, 0x0, 0x0, // code = CodeLogin
		0x8, 0x0, 0x0, 0x0, // len("testuser")
		0x74, 0x65, 0x73, 0x74, 0x75, 0x73, 0x65, 0x72, // "testuser"
		0x8, 0x0, 0x0, 0x0, // len("testpass")
		0x74, 0x65, 0x73, 0x74, 0x70, 0x61, 0x73, 0x73, // "testpass"
		0xa0, 0x0, 0x0, 0x0, // versionMajor = 160
		0x20, 0x0, 0x0, 0x0, // len(md5 hex sum) = 32
		0x65, 0x36, 0x66, 0x34, 0x63, 0x32, 0x35, 0x37, 0x30, 0x64, 0x33, 0x30,
		0x65, 0x66, 0x33, 0x61, 0x62, 0x65, 0x32, 0x63, 0x63, 0x33, 0x30, 0x66,
		0x32, 0x62, 0x38, 0x30, 0x66, 0x30, 0x31, 0x64, // md5("testusertestpass") hex
		0x1, 0x0, 0x0, 0x0, // versionMinor = 1
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("Serialize() = %#v, want %#v", got, want)
	}
}

// buildLoginResponse writes size(4) | code(4) | rest into a buffer, where
// size is computed to cover code(4)+len(rest) unless overridden.
func buildLoginResponse(t *testing.T, code uint32, rest []byte) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	if err := internal.WriteUint32(buf, uint32(4+len(rest))); err != nil {
		t.Fatalf("write size: %v", err)
	}

	if err := internal.WriteUint32(buf, code); err != nil {
		t.Fatalf("write code: %v", err)
	}

	buf.Write(rest)

	return buf.Bytes()
}

func TestLoginDeserializeSuccess(t *testing.T) {
	rest := new(bytes.Buffer)
	if err := internal.WriteBool(rest, true); err != nil {
		t.Fatalf("write success: %v", err)
	}

	if err := internal.WriteString(rest, "welcome"); err != nil {
		t.Fatalf("write greet: %v", err)
	}

	ip := net.ParseIP("1.2.3.4").To4()
	ipValue := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	if err := internal.WriteUint32(rest, ipValue); err != nil {
		t.Fatalf("write ip: %v", err)
	}

	if err := internal.WriteString(rest, "deadbeef"); err != nil {
		t.Fatalf("write sum: %v", err)
	}

	frame := buildLoginResponse(t, uint32(CodeLogin), rest.Bytes())

	l := &Login{}
	if err := l.Deserialize(bytes.NewReader(frame)); err != nil {
		t.Fatalf("Deserialize: unexpected error: %v", err)
	}

	if l.Greet != "welcome" {
		t.Errorf("Greet = %q, want %q", l.Greet, "welcome")
	}

	if !l.IP.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("IP = %v, want 1.2.3.4", l.IP)
	}

	if l.Sum != "deadbeef" {
		t.Errorf("Sum = %q, want %q", l.Sum, "deadbeef")
	}
}

func TestLoginDeserializeFailures(t *testing.T) {
	tests := []struct {
		name       string
		errMessage string
		wantErr    error
	}{
		{"invalid pass", ErrInvalidPass.Error(), ErrInvalidPass},
		{"invalid username", ErrInvalidUsername.Error(), ErrInvalidUsername},
		{"invalid version", ErrInvalidVersion.Error(), ErrInvalidVersion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest := new(bytes.Buffer)
			if err := internal.WriteBool(rest, false); err != nil {
				t.Fatalf("write success: %v", err)
			}

			if err := internal.WriteString(rest, tt.errMessage); err != nil {
				t.Fatalf("write err message: %v", err)
			}

			frame := buildLoginResponse(t, uint32(CodeLogin), rest.Bytes())

			l := &Login{}
			err := l.Deserialize(bytes.NewReader(frame))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoginDeserializeUnknownFailure(t *testing.T) {
	rest := new(bytes.Buffer)
	if err := internal.WriteBool(rest, false); err != nil {
		t.Fatalf("write success: %v", err)
	}

	if err := internal.WriteString(rest, "SOMETHINGELSE"); err != nil {
		t.Fatalf("write err message: %v", err)
	}

	frame := buildLoginResponse(t, uint32(CodeLogin), rest.Bytes())

	l := &Login{}
	err := l.Deserialize(bytes.NewReader(frame))
	if err == nil {
		t.Fatal("Deserialize: expected error, got nil")
	}

	if errors.Is(err, ErrInvalidPass) || errors.Is(err, ErrInvalidUsername) || errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("err = %v, want an unknown-failure error, not a known sentinel", err)
	}
}

func TestLoginDeserializeMismatchingCode(t *testing.T) {
	frame := buildLoginResponse(t, uint32(CodeLogin)+1, []byte{0x0})

	l := &Login{}
	err := l.Deserialize(bytes.NewReader(frame))
	if !errors.Is(err, soul.ErrMismatchingCodes) {
		t.Fatalf("err = %v, want ErrMismatchingCodes", err)
	}
}
