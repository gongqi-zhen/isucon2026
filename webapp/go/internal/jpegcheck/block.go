// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jpegcheck

// image/jpeg の dct.go から、型と定数だけを持ってきた（逆 DCT は使わない）。

type block [blockSize]int32

const blockSize = 8 * 8
