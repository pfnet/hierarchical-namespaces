package utils

import "testing"

func TestScopedRQName(t *testing.T) {
	tests := []struct {
		name         string
		hrqNamespace string
		hrqName      string
		want         string
	}{
		{
			name:         "short",
			hrqNamespace: "default",
			hrqName:      "test",
			want:         "hrq.hnc.x-k8s.io-default-test-1b5cb9615ea99c0edaf5b1f157ce3997",
		},
		{
			name:         "too_long",
			hrqNamespace: "default",
			hrqName:      "12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890",
			want:         "hrq.hnc.x-k8s.io-default-123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345-dc2d54f49ea75bc9da92c7272bc626d7", // 253 chars
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ScopedRQName(test.hrqNamespace, test.hrqName)
			if err != nil {
				t.Errorf("ScopedRQName(%s, %s) = %v", test.hrqNamespace, test.hrqName, err)
			}
			if got != test.want {
				t.Errorf("ScopedRQName(%s, %s) = %s, want %s", test.hrqNamespace, test.hrqName, got, test.want)
			}
		})
	}
}
