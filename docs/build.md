# Go アプリのビルド（#112）

asia1 は CPU が律速なので（#71）、ビルドの設定で CPU を減らす。どちらもアプリの出力（応答）は変わらない。

## 本番向けのバイナリを作る

手元の Mac で、repo のルートから実行する（#21 の手順と同じ。`GOAMD64=v3` を足した）。

```bash
(cd webapp/go && go test ./... && make linux)
```

`make linux` は次と同じ。

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOAMD64=v3 go build -o ../../isureal.linux ./cmd/isureal
```

配り方は #21 の「共通：アプリのデプロイ」のとおり（`isureal.prev` を残して rsync し、`isureal-go` を再起動する）。

## GOAMD64=v3

`GOAMD64=v3` にすると、コンパイラは AVX2、BMI2、FMA などの命令を使う。既定の v1 と比べて、算術とコピーが少し速くなる（数 % 以下）。

**AVX2 のない CPU では起動時に落ちる。** 配る前に、4台すべてで次を実行し、1 以上が出ることを確かめる。

```bash
for h in asia1 asia2 europe1 america1; do echo "== $h"; ssh $h 'grep -c avx2 /proc/cpuinfo'; done
```

0 が出るサーバがあれば、`GOAMD64=v3` を外して（既定の v1 で）ビルドする。追試でサーバが作り直されても、同じインスタンスタイプなら同じ CPU の世代になる。

## PGO（profile-guided optimization）

`webapp/go/cmd/isureal/default.pgo` にプロファイルを置くと、`go build` が自動で使う（Go 1.21 以降の `-pgo=auto`）。よく呼ばれる関数のインライン化と、インターフェースの呼び出しの具体化が進み、典型的には 2〜7% の CPU が減る。

プロファイルは、負荷走行中の asia1 のものを使う（pprof は `ISUREAL_PPROF_ADDR`。既定は `off` なので、取る前に asia1 の `~/env.sh` に `ISUREAL_PPROF_ADDR=127.0.0.1:6060` を足して `isureal-go` を再起動し、取り終えたら外して再起動する。#2）。

1. 負荷走行を始めてから、asia1 で 30 秒のプロファイルを取る。
   ```bash
   ssh asia1 'curl -s "http://127.0.0.1:6060/debug/pprof/profile?seconds=30" -o /tmp/isureal.pprof'
   ```
   手元に `go tool pprof` があれば、次でも同じ（`-proto` で生のプロファイルを出す）。
   ```bash
   ssh asia1 'go tool pprof -proto http://127.0.0.1:6060/debug/pprof/profile?seconds=30' > webapp/go/cmd/isureal/default.pgo
   ```
2. `webapp/go/cmd/isureal/default.pgo` に置き、repo にコミットする（数百 KB）。
   ```bash
   scp asia1:/tmp/isureal.pprof webapp/go/cmd/isureal/default.pgo
   ```
3. 上の手順でビルドし直して配る。`go version -m isureal.linux | grep pgo` で、使われたことを確かめられる。

注意：

- プロファイルは、取ったときのコードから多少変わっても使える（関数名で対応づける）。大きく書き換えたら取り直す。
- 今は `default.pgo` を置いていない（実機でしか取れないため）。置くまでは、PGO なしでビルドされる。
- 走行中でないときのプロファイルは、アイドルの goroutine ばかりになり役に立たない。

## 確かめ方

変更の前後で、負荷走行のスコアと、asia1 の `pidstat -u 5` の isureal の CPU を比べる。

関連：#71、#21、#112
