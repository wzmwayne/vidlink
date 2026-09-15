package sm3

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 标准测试向量来自 GB/T 32907-2016 附录 A。
func TestSumStandardVectors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "abc",
			in:   "abc",
			want: "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0",
		},
		{
			name: "abcd x16 (64 bytes, 两个分组)",
			in:   strings.Repeat("abcd", 16),
			want: "debe9ff92275b8a138604889c18e5a4d6fdb70e5387e5765293dcba39c0c5732",
		},
		{
			name: "空串",
			in:   "",
			want: "1ab21d8355cfa17f8e61194831e81a8f22bec8c728fefb747ed035eb5082aa2b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hex.EncodeToString(func() []byte { b := Sum([]byte(tc.in)); return b[:] }())
			if got != tc.want {
				t.Fatalf("Sum(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// 覆盖各种边界长度，确保 padding 与分组压缩在 55/56/63/64/119/120 字节处正确。
func TestSumBlockBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 54, 55, 56, 57, 63, 64, 65, 118, 119, 120, 127, 128, 129} {
		in := strings.Repeat("x", n)
		oneShot := Sum([]byte(in))

		var d digest
		d.Reset()
		// 分片写入，模拟流式场景，结果必须一致。
		for i := 0; i < len(in); i += 7 {
			end := i + 7
			if end > len(in) {
				end = len(in)
			}
			_, _ = d.Write([]byte(in[i:end]))
		}
		streamed := d.check()

		if oneShot != streamed {
			t.Fatalf("长度 %d: 一次性=%x 流式=%x", n, oneShot, streamed)
		}
	}
}

func TestDouble(t *testing.T) {
	// Double(x) 必须等于 Sum(Sum(x))。
	first := Sum([]byte("hello"))
	want := Sum(first[:])
	if got := Double([]byte("hello")); got != want {
		t.Fatalf("Double 与 Sum(Sum) 不一致: %x vs %x", got, want)
	}
}

func TestReverse(t *testing.T) {
	var in [Size]byte
	for i := range in {
		in[i] = byte(i)
	}
	out := Reverse(in)
	for i := range out {
		if out[i] != byte(Size-1-i) {
			t.Fatalf("Reverse 在位置 %d 错误", i)
		}
	}
	// 逆序两次应还原。
	if Reverse(out) != in {
		t.Fatal("Reverse 两次未还原")
	}
}
