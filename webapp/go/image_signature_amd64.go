package isureal

import "golang.org/x/sys/cpu"

//go:generate go run image_signature_x8_gen.go

// hmacChainX8AVX512 は hmacChainGeneric と同じ計算を、AVX-512VL で8本同時に行う（image_signature_amd64.s）。
//
//go:noescape
func hmacChainX8AVX512(st *hmacChainState, iterations int)

func init() {
	if cpu.X86.HasAVX512F && cpu.X86.HasAVX512VL {
		hmacChainX8 = hmacChainX8AVX512
		hmacChainX8Available = true
	}
}
