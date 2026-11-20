# /// script
# requires-python = ">=3.10"
# dependencies = ["mcp>=1.2,<2"]
# ///
"""dsh MCP bridge for the local comfyui-server.

Exposes the comfyui-server REST API (docs/AI-API.md) as MCP tools so a dsh
agent can generate images through the existing image service.

Transport: stdio (dsh-mcp-client launches `uv run <this file>`).

Env:
  COMFY_BASE       comfyui-server backend URL. Defaults to the NAS service.
  COMFY_UI_TARGET  Optional ComfyUI address the backend should proxy to.
                   When set, the bridge applies it at startup if it differs
                   from the backend's current target. Defaults to empty.

This file parses under Python 3.9 (system python) for `--selftest`, which only
performs read-only HTTP calls and never imports the mcp package or mutates the
backend.
"""
from __future__ import annotations

import os
import sys
import json
import base64
import functools
import urllib.request
import urllib.error
import urllib.parse

# The comfyui-server the agent talks to. Default is the NAS instance reached
# directly over its DDNS hostname (IPv6) — 28 production workflows, and it owns
# its own ComfyUI target. Override with COMFY_BASE (e.g. a local backend).
COMFY_BASE = os.environ.get("COMFY_BASE", "http://nas.youngam.cn:40016").rstrip("/")
# ComfyUI address to auto-apply to the backend on startup. Empty by default:
# the NAS backend already targets its own local ComfyUI, so we do not touch it.
# Set COMFY_UI_TARGET only if you want this bridge to manage the backend target.
COMFY_UI_TARGET = os.environ.get("COMFY_UI_TARGET", "").strip()

DEFAULT_TIMEOUT = 20  # seconds; submit/poll endpoints are quick, no auto-wait here


def _log(msg):
    # stdout is reserved for the JSON-RPC stream; diagnostics go to stderr only.
    print("[comfyui-mcp] " + msg, file=sys.stderr, flush=True)


def _http(method, path, body=None, timeout=DEFAULT_TIMEOUT):
    """Return (status_code, response_bytes, response_headers). Raise ConnectionError on transport failure."""
    url = COMFY_BASE + path
    data = None
    headers = {"Accept": "application/json"}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as e:
        # HTTP-level error still carries a body ({"error": "..."}).
        return e.code, e.read(), dict(getattr(e, "headers", {}) or {})
    except urllib.error.URLError as e:
        raise ConnectionError("%s %s failed: %s" % (method, url, e.reason))


def _jload(raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        return {"_raw": raw.decode("utf-8", "replace")[:500]}


class BackendHTTPError(RuntimeError):
    pass


def _check_status(status, raw, allowed=()):
    if 200 <= status < 300 or status in allowed:
        return
    result = _jload(raw)
    details = (result.get("error") or result.get("_raw")) if isinstance(result, dict) else result
    raise BackendHTTPError("HTTP %s%s" % (status, ": " + str(details)[:500] if details else ""))


def _api_json(method, path, body=None, allowed=()):
    status, raw, _headers = _http(method, path, body=body)
    _check_status(status, raw, allowed)
    result = _jload(raw)
    if not isinstance(result, dict) or "_raw" in result:
        raise BackendHTTPError("后端响应不是有效的 JSON 对象")
    return result


# ---- read-only / management helpers (used by both selftest and tools) --------

def health():
    return _api_json("GET", "/api/health")


def get_target():
    return _api_json("GET", "/api/settings")


def test_target(url=None):
    path = "/api/settings/comfyui/test"
    if url:
        path += "?url=" + urllib.parse.quote(url, safe="")
    return _api_json("POST", path, body={}, allowed=(502,))


def set_target(url):
    return _api_json("PUT", "/api/settings/comfyui", body={"url": url})


def list_workflows():
    workflows = {}
    page, page_size = 1, 200
    while True:
        result = _api_json("GET", "/api/workflows?page=%s&page_size=%s" % (page, page_size))
        items = result["items"]
        workflows.update((item["id"], item) for item in items)
        if len(items) < page_size or page * page_size >= result["total"]:
            return {"items": list(workflows.values()), "total": result["total"]}
        page += 1


def resolve_workflow(workflow_id=None):
    j = list_workflows()
    items = j.get("items") or []
    if workflow_id is not None:
        for it in items:
            if it.get("id") == workflow_id:
                if not it.get("enabled", True):
                    raise ValueError("workflow %s is disabled" % workflow_id)
                return workflow_id
        raise ValueError("workflow %s not found" % workflow_id)
    for it in items:
        if it.get("is_default") and it.get("enabled", True):
            return it["id"]
    for it in items:
        if it.get("enabled", True):
            return it["id"]
    raise ValueError("no enabled workflow available")


def submit(positive, workflow_id=None, negative=None, title=None):
    wid = resolve_workflow(workflow_id)
    body = {"workflow_id": wid, "positive_prompt": positive}
    if negative:
        body["negative_prompt"] = negative
    if title:
        body["title"] = title
    s, raw, _h = _http("POST", "/api/tasks/direct", body=body)
    return s, _jload(raw)


def task_status(task_id):
    return _api_json("GET", "/api/tasks/%s" % task_id)


def image_bytes(image_id, full=False):
    path = "/api/images/%s/%s" % (image_id, "file" if full else "preview")
    status, raw, headers = _http("GET", path, timeout=60)
    _check_status(status, raw)
    mime = next((v for k, v in headers.items() if k.lower() == "content-type"), "")
    mime = mime.partition(";")[0].strip().lower()
    if not mime.startswith("image/") or not raw:
        raise BackendHTTPError("取图响应没有有效的图片")
    return raw, mime


def apply_default_target():
    """Best-effort: point the backend at COMFY_UI_TARGET only if that target is
    actually reachable from here. A dead default must never clobber a working
    value the user set manually (e.g. NAS LAN IP vs DDNS hostname)."""
    if not COMFY_UI_TARGET:
        _log("COMFY_UI_TARGET empty; leaving backend comfyui_url untouched")
        return
    try:
        cur = (get_target().get("comfyui_url") or "").strip()
    except (ConnectionError, BackendHTTPError) as e:
        _log("cannot read backend target (backend down?): %s" % e)
        return
    if cur == COMFY_UI_TARGET:
        return
    # Only write when the configured target responds; otherwise keep current.
    try:
        chk = test_target(COMFY_UI_TARGET)
    except (ConnectionError, BackendHTTPError) as e:
        _log("cannot test target %s (backend down?): %s; leaving %s" % (COMFY_UI_TARGET, e, cur))
        return
    if not chk.get("reachable"):
        _log("target %s not reachable from here; keeping backend at %s" % (COMFY_UI_TARGET, cur or "<empty>"))
        return
    try:
        res = set_target(COMFY_UI_TARGET)
        _log("backend comfyui_url: %s -> %s" % (cur or "<empty>", res.get("comfyui_url")))
    except (ConnectionError, BackendHTTPError) as e:
        _log("failed to apply comfyui_url: %s" % e)


# ---- selftest (Python 3.9 safe, read-only, no mcp import) --------------------

def selftest():
    import urllib.parse  # noqa: F401  (ensures attr available if any helper needs it)
    ok = True
    try:
        _log("health: %s" % health())
    except (ConnectionError, BackendHTTPError) as e:
        ok = False
        _log("health FAILED (backend unreachable at %s): %s" % (COMFY_BASE, e))
    try:
        _log("target: %s" % get_target())
    except (ConnectionError, BackendHTTPError) as e:
        ok = False
        _log("target FAILED: %s" % e)
    try:
        _log("test_target: %s" % test_target())
    except (ConnectionError, BackendHTTPError) as e:
        _log("test_target FAILED (non-fatal if target host offline): %s" % e)
    try:
        j = list_workflows()
        items = j.get("items") or []
        _log("workflows: total=%s sample=%s" % (j.get("total"), [
            {"id": i.get("id"), "name": i.get("name"), "enabled": i.get("enabled"), "default": i.get("is_default")}
            for i in items[:5]]))
        _log("resolve_workflow(None) -> %s" % resolve_workflow(None))
    except (ConnectionError, BackendHTTPError) as e:
        ok = False
        _log("workflows FAILED: %s" % e)
    except ValueError as e:
        ok = False
        _log("workflow resolve: %s" % e)
    _log("selftest %s" % ("OK" if ok else "PROBLEM"))
    return 0 if ok else 1


# ---- MCP server bootstrap (runs only under `uv run`, py>=3.10, mcp present) --

def run_server():
    from mcp.server.fastmcp import FastMCP, Image

    INSTRUCTIONS = (
        "通过 comfyui-server 服务作图。流程:"
        "1) comfy_status 看后端与 ComfyUI 目标是否可达;"
        "2) comfy_list_workflows 选 workflow_id(描述里含尺寸/采样器/模型);"
        "3) comfy_generate(positive, workflow_id?, negative?, title?) 提交,返回 task id;"
        "4) comfy_task(task_id) 每 3-5 秒轮询到 completed/failed/cancelled(单张通常 20-90 秒);"
        "5) comfy_view_image(image_id) 把图取回对话。"
        "仅当 ComfyUI 目标在线时才真正出图;要在对话里看到图,需当前模型支持图像输入。"
    )
    mcp = FastMCP("comfyui", instructions=INSTRUCTIONS)

    def _safe(fn):
        @functools.wraps(fn)
        def wrapper(*args, **kwargs):
            try:
                return fn(*args, **kwargs)
            except ConnectionError as e:
                return "后端不可达: %s" % e
            except ValueError as e:
                return "参数错误: %s" % e
            except Exception as e:  # noqa: BLE001
                return "调用失败 %s: %s" % (type(e).__name__, e)
        return wrapper

    @mcp.tool()
    @_safe
    def comfy_status() -> str:
        """查后端健康、当前 ComfyUI 目标地址,以及该目标是否可达(不耗 GPU、不出图)。"""
        out = {"backend": COMFY_BASE}
        out["health"] = health()
        out["comfyui_url"] = get_target().get("comfyui_url")
        out["target_check"] = test_target()
        return json.dumps(out, ensure_ascii=False, indent=2)

    @mcp.tool()
    @_safe
    def comfy_list_workflows() -> str:
        """列出可用工作流(id/名称/尺寸模型描述/是否默认),供挑选风格与尺寸。"""
        items = (list_workflows().get("items") or [])
        slim = [{
            "id": i.get("id"),
            "name": i.get("name"),
            "description": i.get("description"),
            "enabled": i.get("enabled"),
            "is_default": i.get("is_default"),
        } for i in items if i.get("enabled", True)]
        return json.dumps({"count": len(slim), "workflows": slim}, ensure_ascii=False, indent=2)

    @mcp.tool()
    @_safe
    def comfy_generate(positive: str, workflow_id: int = -1,
                      negative: str = "", title: str = "") -> str:
        """提交一张出图任务,返回 task id(用 comfy_task 轮询)。
        workflow_id=-1 表示自动选(默认项,否则第一个启用)。negative 留空则用工作流自带负面。"""
        wid = None if workflow_id is None or workflow_id < 0 else workflow_id
        s, j = submit(positive, workflow_id=wid,
                      negative=(negative or None), title=(title or None))
        result = {"http": s, "task": j}
        if 200 <= s < 300 and j.get("id") is not None:
            result["hint"] = "用 comfy_task(%s) 轮询到任务结束" % j["id"]
        return json.dumps(result, ensure_ascii=False, indent=2)

    @mcp.tool()
    @_safe
    def comfy_task(task_id: int) -> str:
        """查询任务进度;完成时列出可取的 image_id(用 comfy_view_image 查看)。"""
        j = task_status(task_id)
        status = j.get("status")
        images = []
        items = []
        for it in (j.get("items") or []):
            items.append({"id": it.get("id"), "status": it.get("status"), "error": it.get("error_message")})
            for im in (it.get("images") or []):
                images.append({"image_id": im.get("id"), "filename": im.get("filename"),
                               "width": im.get("width"), "height": im.get("height"),
                               "item_status": it.get("status"),
                               "error": it.get("error_message")})
        hint = ""
        if status in ("pending", "running"):
            if any(item["status"] == "submitted" and item["error"] for item in items):
                hint = "ComfyUI 连接或响应异常，服务正在重试查询"
            else:
                hint = "仍在 %s,3-5 秒后再查" % status
        elif status == "completed":
            if j.get("failed_count"):
                hint = "部分生成项失败，可查看各项错误与已生成的图片"
            else:
                hint = "已完成,用 comfy_view_image 查看" if images else "已完成但无图(看 items 报错)"
        else:
            hint = "终态 %s" % status
        return json.dumps({"task_id": task_id, "status": status,
                           "success_count": j.get("success_count"),
                           "failed_count": j.get("failed_count"),
                           "items": items, "images": images, "hint": hint},
                          ensure_ascii=False, indent=2)

    @mcp.tool()
    def comfy_view_image(image_id: int, full: bool = False):
        """把图发回对话。默认 WebP 预览(长边≤1600);full=true 取原图。
        需当前模型支持图像输入才会显示,否则只回一条诊断文本。"""
        try:
            raw, mime = image_bytes(image_id, full=full)
        except ConnectionError as e:
            return "后端不可达: %s" % e
        except BackendHTTPError as e:
            return "取图失败: %s" % e
        subtype = (mime or "image/webp").split("/", 1)[-1].lower()
        if subtype in ("jpg",):
            subtype = "jpeg"
        return Image(data=raw, format=subtype)

    @mcp.tool()
    @_safe
    def comfy_set_target(url: str) -> str:
        """把后端指向另一个 ComfyUI 地址(host:port),例如 http://nas.youngam.cn:8188。"""
        return json.dumps(set_target(url), ensure_ascii=False)

    @mcp.tool()
    @_safe
    def comfy_test_target(url: str = "") -> str:
        """探测某个 ComfyUI 目标的 /system_stats(不耗 GPU);url 留空则测当前目标。"""
        return json.dumps(test_target(url or None), ensure_ascii=False)

    apply_default_target()
    mcp.run()


if __name__ == "__main__":
    if "--selftest" in sys.argv:
        import urllib.parse  # used by test_target when url passed
        sys.exit(selftest())
    try:
        run_server()
    except ImportError as e:
        _log("mcp package not available in this interpreter: %s" % e)
        _log("start via `uv run comfyui_mcp.py` (uses PEP723 deps).")
        sys.exit(1)
