package bson

import (
	"bytes"
	"testing"
)

func TestRawLen(t *testing.T) {
	r := Raw{0x05, 0x00, 0x00, 0x00, 0x00}
	if got := r.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}
}

func TestRawValidate(t *testing.T) {
	tests := []struct {
		name string
		r    Raw
		want error
	}{
		{"empty doc", Empty, nil},
		{"too short", Raw{0x01, 0x02}, ErrTooShort},
		{"length mismatch", Raw{0x06, 0x00, 0x00, 0x00, 0x00}, ErrLengthMismatch},
		{"missing terminator", Raw{0x05, 0x00, 0x00, 0x00, 0x01}, ErrLengthMismatch},
		{"nil", nil, ErrTooShort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Validate(); got != tt.want {
				t.Fatalf("Validate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRawClone(t *testing.T) {
	orig := Raw{0x05, 0x00, 0x00, 0x00, 0x00}
	c := orig.Clone()
	if !bytes.Equal(orig, c) {
		t.Fatal("clone not equal to original")
	}
	c[0] = 0xFF
	if orig[0] == 0xFF {
		t.Fatal("clone aliases original")
	}
	if Raw(nil).Clone() != nil {
		t.Fatal("clone of nil should be nil")
	}
}

func TestEmptyIsValid(t *testing.T) {
	if err := Empty.Validate(); err != nil {
		t.Fatalf("Empty should validate: %v", err)
	}
	if Empty.Len() != MinDocLen {
		t.Fatalf("Empty.Len = %d, want %d", Empty.Len(), MinDocLen)
	}
}
