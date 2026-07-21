package server

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestPossibleParentsRejectsHugeCount locks the parent-count cap: a (possibly
// compromised) server declaring a huge parent count must be rejected before the
// append loop, rather than driving an unbounded allocation of Parent structs.
func TestPossibleParentsRejectsHugeCount(t *testing.T) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(0))                   // size (unused past read)
	binary.Write(buf, binary.LittleEndian, uint32(CodePossibleParents)) // code 102
	binary.Write(buf, binary.LittleEndian, uint32(1_000_000))           // absurd parent count

	var pp PossibleParents
	err := pp.Deserialize(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("Deserialize accepted an absurd parent count, want rejection")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("err = %v, want a count-exceeds-max rejection", err)
	}
	if len(pp.Parents) != 0 {
		t.Errorf("appended %d parents before rejecting, want 0", len(pp.Parents))
	}
}

// TestPossibleParentsRoundTrip confirms a legitimate small list still decodes.
func TestPossibleParentsRoundTrip(t *testing.T) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(0))                   // size
	binary.Write(buf, binary.LittleEndian, uint32(CodePossibleParents)) // code
	binary.Write(buf, binary.LittleEndian, uint32(1))                   // one parent
	// username "bob"
	binary.Write(buf, binary.LittleEndian, uint32(3))
	buf.WriteString("bob")
	binary.Write(buf, binary.LittleEndian, uint32(0x0100007f)) // IP
	binary.Write(buf, binary.LittleEndian, uint32(2242))       // port

	var pp PossibleParents
	if err := pp.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if len(pp.Parents) != 1 || pp.Parents[0].Username != "bob" || pp.Parents[0].Port != 2242 {
		t.Errorf("decoded %+v, want one parent bob:2242", pp.Parents)
	}
}
