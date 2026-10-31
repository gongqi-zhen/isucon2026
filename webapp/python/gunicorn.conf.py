import os

wsgi_app = "isureal.main:create_app()"
bind = "0.0.0.0:8080"
# 各ワーカーが DB 接続プール（ISUREAL_DB_POOL_SIZE）を持つ
workers = int(os.getenv("ISUREAL_WORKERS", "4"))
worker_class = "uvicorn_worker.UvicornWorker"
forwarded_allow_ips = ""
# 初期化のように長くかかる処理の途中でワーカーが止められないようにする
timeout = 120
