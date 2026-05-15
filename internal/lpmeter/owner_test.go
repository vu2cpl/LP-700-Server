package lpmeter

import "testing"

func TestIsCommandEcho(t *testing.T) {
	tests := []struct {
		name string
		make func() []byte
		want bool
	}{
		{
			name: "true: cmd '1' echo (byte 0 = '1', rest zero)",
			make: func() []byte { r := make([]byte, ReportSize); r[0] = '1'; return r },
			want: true,
		},
		{
			name: "true: cmd '6' echo",
			make: func() []byte { r := make([]byte, ReportSize); r[0] = '6'; return r },
			want: true,
		},
		{
			name: "true: cmd '?' echo (high end of cmd-char range)",
			make: func() []byte { r := make([]byte, ReportSize); r[0] = '?'; return r },
			want: true,
		},
		{
			name: "false: byte 0 in cmd-char range but byte N != 0",
			make: func() []byte {
				r := make([]byte, ReportSize)
				r[0] = '1'
				r[7] = 0xff
				return r
			},
			want: false,
		},
		{
			name: "false: real telemetry — byte 0 = 0 (typical)",
			make: func() []byte {
				r := make([]byte, ReportSize)
				r[3] = 1 // top_mode
				r[6] = 5 // range
				return r
			},
			want: false,
		},
		{
			name: "false: sample frame — byte 0 = 0x97 (151, outside cmd-char range)",
			make: func() []byte {
				r := make([]byte, ReportSize)
				for i := range r {
					r[i] = 0x97
				}
				return r
			},
			want: false,
		},
		{
			name: "false: empty/zero frame (byte 0 = 0, not in cmd range)",
			make: func() []byte { return make([]byte, ReportSize) },
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCommandEcho(tt.make())
			if got != tt.want {
				t.Errorf("isCommandEcho: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllFilled(t *testing.T) {
	if !allFilled([]bool{true, true, true, true, true}) {
		t.Error("five trues should report all filled")
	}
	if allFilled([]bool{true, true, true, true, false}) {
		t.Error("any false should report not filled")
	}
	if allFilled([]bool{false, false, false, false, false}) {
		t.Error("all false should report not filled")
	}
	if !allFilled([]bool{}) {
		t.Error("empty slice trivially satisfies all-filled")
	}
}
