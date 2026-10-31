ISUREAL_REGION=asia
ISUREAL_DB_HOST=192.168.4.10
ISUREAL_DB_PORT=5432
ISUREAL_DB_USER=isureal
ISUREAL_DB_PASSWORD=isureal
ISUREAL_DB_NAME=isureal
ISUREAL_LOG_LEVEL=info
# asia1 の値。europe1 と america1 の env.sh は 40（合計 104。asia2 のメモリに収める）。
# asia1 = 40、europe1 = america1 = 32 はスコアが下がった（#125、docs/metrics.md）。
ISUREAL_DB_MAX_CONNS=24
