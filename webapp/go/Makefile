.PHONY: isureal linux clean

# GOAMD64=v3：本番の4台は x86_64 で AVX2 に対応している（配る前に `grep -c avx2 /proc/cpuinfo` で確かめる。docs/build.md）。
# 既定の v1 より算術とコピーの命令が少し良くなる。amd64 以外（手元の Mac など）では無視される。
# PGO：cmd/isureal/default.pgo があれば go build が自動で使う（-pgo=auto）。取り方は docs/build.md。
isureal:
	GOAMD64=v3 go build -o isureal cmd/isureal/main.go

# 手元で本番（linux/amd64）向けのバイナリを作る（#21 のデプロイ手順と同じ）。repo のルートに isureal.linux ができる。
linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOAMD64=v3 go build -o ../../isureal.linux ./cmd/isureal

clean:
	rm -f isureal
