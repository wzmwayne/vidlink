// Package sm3 实现 GB/T 32907-2016《SM3 密码杂凑算法》。
//
// 为什么项目里需要它：抖音 Web 端的 a_bogus 签名把「请求参数 + 固定盐 'cus'」
// 与「HTTP 方法 + 'cus'」各做两次 SM3，再用结果字节参与 RC4 载荷构造。
// 这是纯计算，可以完全在 Go 里复现，无需 JS 引擎或浏览器。
package sm3

import (
	"encoding/binary"
	"encoding/hex"
	"math/bits"
)

// Size 是 SM3 摘要的字节长度。
const Size = 32

// BlockSize 是 SM3 的分组字节长度。
const BlockSize = 64

var iv = [8]uint32{
	0x7380166f, 0x4914b2b9, 0x172442d7, 0xda8a0600,
	0xa96f30bc, 0x163138aa, 0xe38dee4d, 0xb0fb0e4e,
}

// Sum 计算 data 的 SM3 摘要。
func Sum(data []byte) [Size]byte {
	var d digest
	d.Reset()
	d.Write(data)
	return d.check()
}

// SumString 计算字符串的 SM3 摘要。
func SumString(s string) [Size]byte { return Sum([]byte(s)) }

// SumHex 返回小写十六进制摘要。
func SumHex(data []byte) string {
	b := Sum(data)
	return hex.EncodeToString(b[:])
}

// Double 连续做两次 SM3：SM3(SM3(data))。
// a_bogus 使用的正是这个双重哈希。
func Double(data []byte) [Size]byte {
	first := Sum(data)
	return Sum(first[:])
}

// DoubleHex 返回双重 SM3 的小写十六进制。
func DoubleHex(data []byte) string {
	b := Double(data)
	return hex.EncodeToString(b[:])
}

// Reverse 返回摘要的逆序副本。
//
// a_bogus 把 UA 的 SM3 摘要按字节倒序后作为固定指纹参与载荷构造，
// 因此这里显式提供 Reverse。
func Reverse(b [Size]byte) [Size]byte {
	var out [Size]byte
	for i := range b {
		out[i] = b[Size-1-i]
	}
	return out
}

type digest struct {
	h   [8]uint32
	buf [BlockSize]byte
	n   int
	len uint64 // 已写入的总字节数
}

// Reset 复位到初始状态。
func (d *digest) Reset() {
	d.h = iv
	d.n = 0
	d.len = 0
}

// Write 追加数据，实现 io.Writer。
func (d *digest) Write(p []byte) (int, error) {
	written := len(p)
	d.len += uint64(len(p))

	if d.n > 0 {
		c := copy(d.buf[d.n:], p)
		d.n += c
		if d.n < BlockSize {
			return written, nil
		}
		d.compress(d.buf[:])
		d.n = 0
		p = p[c:]
	}
	for len(p) >= BlockSize {
		d.compress(p[:BlockSize])
		p = p[BlockSize:]
	}
	if len(p) > 0 {
		d.n = copy(d.buf[:], p)
	}
	return written, nil
}

// check 完成 padding 并输出摘要，不改变 d 的已写入数据。
func (d *digest) check() [Size]byte {
	var tmp digest
	tmp.h = d.h
	tmp.buf = d.buf
	tmp.n = d.n
	tmp.len = d.len

	var length [8]byte
	binary.BigEndian.PutUint64(length[:], tmp.len<<3)

	tmp.Write([]byte{0x80}) // 注意：这里会给 len 多加 1 字节，
	// 但我们预先算好了 length，所以用临时副本没关系。
	for tmp.n != 56 {
		tmp.Write([]byte{0x00})
	}
	// 直接写入长度，绕过 Write 的计数逻辑。
	copy(tmp.buf[56:], length[:])
	tmp.compress(tmp.buf[:])

	var out [Size]byte
	for i, v := range tmp.h {
		binary.BigEndian.PutUint32(out[i*4:], v)
	}
	return out
}

func (d *digest) compress(p []byte) {
	var w [68]uint32
	var wp [64]uint32

	for i := 0; i < 16; i++ {
		w[i] = binary.BigEndian.Uint32(p[i*4:])
	}
	for i := 16; i < 68; i++ {
		w[i] = p1(w[i-16]^w[i-9]^bits.RotateLeft32(w[i-3], 15)) ^
			bits.RotateLeft32(w[i-13], 7) ^ w[i-6]
	}
	for i := 0; i < 64; i++ {
		wp[i] = w[i] ^ w[i+4]
	}

	a, b, c, e, f, g, h := d.h[0], d.h[1], d.h[2], d.h[4], d.h[5], d.h[6], d.h[7]
	dd := d.h[3]

	for j := 0; j < 64; j++ {
		var t uint32
		if j < 16 {
			t = 0x79cc4519
		} else {
			t = 0x7a879d8a
		}
		rot := uint(j % 32)

		a12 := bits.RotateLeft32(a, 12)
		ss1 := bits.RotateLeft32(a12+e+bits.RotateLeft32(t, int(rot)), 7)
		ss2 := ss1 ^ a12

		var ff, gg uint32
		if j < 16 {
			ff = a ^ b ^ c
			gg = e ^ f ^ g
		} else {
			ff = (a & b) | (a & c) | (b & c)
			gg = (e & f) | (^e & g)
		}

		tt1 := ff + dd + ss2 + wp[j]
		tt2 := gg + h + ss1 + w[j]

		dd = c
		c = bits.RotateLeft32(b, 9)
		b = a
		a = tt1
		h = g
		g = bits.RotateLeft32(f, 19)
		f = e
		e = p0(tt2)
	}

	d.h[0] ^= a
	d.h[1] ^= b
	d.h[2] ^= c
	d.h[3] ^= dd
	d.h[4] ^= e
	d.h[5] ^= f
	d.h[6] ^= g
	d.h[7] ^= h
}

func p0(x uint32) uint32 { return x ^ bits.RotateLeft32(x, 9) ^ bits.RotateLeft32(x, 17) }
func p1(x uint32) uint32 { return x ^ bits.RotateLeft32(x, 15) ^ bits.RotateLeft32(x, 23) }
