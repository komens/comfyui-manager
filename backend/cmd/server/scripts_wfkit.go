package main

// wfkitSource 是随每次运行写进工作目录的 wfkit.py。
//
// 为什么要有这个文件：脚本里最常做的四件事——查库、定位图片、调接口、存产出——
// 每次都手写一遍 sqlite3/urllib 既啰嗦又容易踩坑（比如 storage_path 可能是绝对路径，
// 直接 join(DATA_DIR,'images',...) 会找不到）。这里把它固化成一份小工具，
// 顺带把「库只读、写走接口」这条约定写进代码里。
const wfkitSource = `"""wfkit —— 脚本运行台附带的工具箱（纯标准库，不需要 pip install）。

常用：
    import wfkit

    for row in wfkit.query("SELECT id, storage_path FROM images ORDER BY id DESC LIMIT 5"):
        print(row["id"], wfkit.image_file(row))

    wfkit.save_output("result.json", {"ok": True})
    wfkit.api("DELETE", "/api/images/12")

数据库是只读的（mode=ro）：服务端用 WAL + 写锁排队，脚本里跑长事务会把提交卡死，
而且直接改行会绕过收藏保护和回收站。要动数据请用 wfkit.api()。
"""

import json
import os
import sqlite3
import sys
import urllib.error
import urllib.request

DATA_DIR = os.environ.get("DATA_DIR", "")
DB_PATH = os.environ.get("DB_PATH", "")
API_BASE = os.environ.get("API_BASE", "http://127.0.0.1:8080")
RUN_DIR = os.environ.get("RUN_DIR", os.getcwd())
OUT_DIR = os.path.join(RUN_DIR, "out")
ARGS = sys.argv[1:]


def connect():
    """打开只读连接。需要写库说明思路错了，请改用 api()。"""
    if not DB_PATH:
        raise RuntimeError("DB_PATH 未设置")
    conn = sqlite3.connect("file:%s?mode=ro" % DB_PATH, uri=True, timeout=5)
    conn.row_factory = sqlite3.Row
    return conn


def query(sql, *params):
    """执行查询，返回 list[dict]。"""
    conn = connect()
    try:
        return [dict(row) for row in conn.execute(sql, params).fetchall()]
    finally:
        conn.close()


def one(sql, *params):
    """只要第一行，没有就返回 None。"""
    rows = query(sql, *params)
    return rows[0] if rows else None


def data_path(*parts):
    """拼 DATA_DIR 下的路径。"""
    return os.path.join(DATA_DIR, *parts)


def data_file(row, subdir):
    """把库里存的 storage_path 解析成真实路径。

    规则与服务端 dataFilePath 一致：先按 DATA_DIR/<subdir>/<basename> 找，
    再退回绝对路径。storage_path 可能是相对路径或绝对路径，直接 join 会找不到。
    """
    stored = ""
    if isinstance(row, dict):
        stored = row.get("storage_path") or ""
    elif isinstance(row, sqlite3.Row):
        stored = row["storage_path"] or ""
    else:
        stored = row or ""
    stored = str(stored).strip()
    if not stored:
        return ""
    candidate = os.path.join(DATA_DIR, subdir, os.path.basename(stored))
    if os.path.isfile(candidate):
        return candidate
    if os.path.isabs(stored) and os.path.isfile(stored):
        return stored
    return ""


def image_file(row):
    """图片的真实路径（row 需要含 storage_path）。"""
    return data_file(row, "images")


def json_files():
    """json_files 表里每条记录 + 解析后的真实路径。"""
    rows = query("SELECT id, filename, storage_path, created_at FROM json_files ORDER BY id DESC")
    for row in rows:
        row["path"] = data_file(row, "json")
    return rows


def save_output(name, data):
    """把产出写进本次运行的 out/ 目录。

    str 直接写文本；bytes 写二进制；其它（dict/list）自动 json 序列化。
    返回写出的绝对路径。
    """
    os.makedirs(OUT_DIR, exist_ok=True)
    target = os.path.join(OUT_DIR, os.path.basename(str(name)))
    if isinstance(data, (bytes, bytearray)):
        with open(target, "wb") as handle:
            handle.write(bytes(data))
    elif isinstance(data, str):
        with open(target, "w", encoding="utf-8") as handle:
            handle.write(data)
    else:
        with open(target, "w", encoding="utf-8") as handle:
            json.dump(data, handle, ensure_ascii=False, indent=2)
    log("saved output: %s" % target)
    return target


def api(method, path, body=None, timeout=60):
    """调本服务的 HTTP 接口（写操作的唯一正道）。

    body 为 dict/list 时按 JSON 发送。非 2xx 会抛 RuntimeError，并带上服务端原话。
    """
    url = API_BASE + path
    payload = None
    headers = {}
    if body is not None:
        payload = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=payload, method=method.upper(), headers=headers)
    # 容器里通常没有代理；显式清空是防止宿主机代理变量漏进来
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=timeout) as response:
            raw = response.read()
            return json.loads(raw.decode("utf-8")) if raw else None
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", "replace")
        raise RuntimeError("%s %s -> HTTP %s: %s" % (method.upper(), path, error.code, detail))


def log(message):
    """带前缀打印，方便在一大段输出里认出自己的日志。"""
    print("[wfkit] %s" % message, flush=True)
`
