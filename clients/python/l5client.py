"""L5 记忆库的 Python 客户端（只用标准库，复制到任何项目里就能用）。

连接顺序：本机 Unix socket（L5_SOCKET，默认 /run/l5mem/l5.sock，免令牌）→ HTTP（L5_URL + L5_TOKEN）。

    from l5client import L5
    l5 = L5(caller="infocollector")
    l5.add("GPU 租赁价格", "H100 云上租赁约 2 美元/小时", kind="fact", tags=["算力", "GPU"], source="https://example.com")
    for h in l5.search("租赁价格", limit=5)["hits"]:
        print(h["id"], h["topic"], h["body"])
"""

from __future__ import annotations

import http.client
import json
import os
import socket
import urllib.parse

DEFAULT_SOCKET = "/run/l5mem/l5.sock"


class L5Error(RuntimeError):
    def __init__(self, status: int, message: str):
        super().__init__(f"L5 {status}：{message}")
        self.status = status


class _UnixConn(http.client.HTTPConnection):
    def __init__(self, path: str, timeout: float):
        super().__init__("l5", timeout=timeout)
        self._path = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self._path)
        self.sock = s


class L5:
    def __init__(self, caller: str = "", url: str = "", token: str = "", sock: str = "", timeout: float = 15):
        self.caller = caller or os.getenv("L5_CALLER") or "python"
        self.sock = sock or os.getenv("L5_SOCKET") or DEFAULT_SOCKET
        self.url = (url or os.getenv("L5_URL") or "").rstrip("/")
        self.token = token or os.getenv("L5_TOKEN", "")
        self.timeout = timeout

    def _conn(self):
        if not self.url and os.path.exists(self.sock):
            return _UnixConn(self.sock, self.timeout), ""
        u = urllib.parse.urlsplit(self.url or "http://127.0.0.1:19110")
        cls = http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection
        return cls(u.hostname, u.port, timeout=self.timeout), u.path.rstrip("/")

    def call(self, op: str, query: dict | None = None, body: dict | None = None):
        q = {k: ("1" if v is True else v) for k, v in (query or {}).items() if v not in (None, "", False)}
        q["caller"] = self.caller
        conn, prefix = self._conn()
        path = f"{prefix}/api/v1/{op}?{urllib.parse.urlencode(q)}"
        headers = {"X-L5-Caller": self.caller}
        if self.token:
            headers["X-L5-Token"] = self.token
        try:
            if body is None:
                conn.request("GET", path, headers=headers)
            else:
                headers["Content-Type"] = "application/json"
                conn.request("POST", path, body=json.dumps(body, ensure_ascii=False).encode(), headers=headers)
            resp = conn.getresponse()
            data = resp.read()
        except OSError as exc:
            raise L5Error(0, f"连不上 L5 服务：{exc}") from exc
        finally:
            conn.close()
        try:
            obj = json.loads(data or b"null")
        except ValueError:
            obj = {"error": data[:200].decode("utf-8", "replace")}
        if resp.status >= 400:
            raise L5Error(resp.status, (obj or {}).get("error", str(resp.status)))
        return obj

    # ---- 写 ----
    def add(self, topic: str, body: str, kind: str = "fact", tags=None, source: str = "", volatility: str = "",
            unverified: bool = False, supersedes=None, meta: dict | None = None, created_at: str = "",
            recheck_days: int = 0) -> dict:
        """返回 {"id", "existed", "volatility", "superseded"}；内容相同重复写入返回 existed=True。
        created_at（"YYYY-MM-DD HH:MM:SS"，本地时间）：补录旧资料时用资料本来的时间。
        recheck_days：自定多少天后到期核对（例如「60 天内见分晓」的假设填 60）。"""
        return self.call("add", body={"topic": topic, "body": body, "kind": kind, "tags": list(tags or []), "source": source,
                                      "volatility": volatility, "unverified": unverified,
                                      "supersedes": [int(x) for x in (supersedes or [])], "meta": meta or {},
                                      "caller": self.caller, **({"created_at": created_at} if created_at else {}),
                                      **({"recheck_days": int(recheck_days)} if recheck_days else {})})

    def supersede(self, old: int, new: int) -> dict:
        return self.call("supersede", body={"old": int(old), "new": int(new)})

    def retract(self, id: int, reason: str = "") -> dict:
        return self.call("retract", body={"id": int(id), "reason": reason})

    def verify(self, id: int, note: str = "", by: str = "") -> dict:
        return self.call("verify", body={"id": int(id), "note": note, "by": by or self.caller})

    # ---- 查 ----
    def search(self, q: str, limit: int = 8, kind: str = "", tag: str = "", topic_prefix: str = "",
               history: bool = False, explain: bool = False, since: str = "", until: str = "",
               no_fold: bool = False) -> dict:
        """since / until：「2026-11-01」「2026-11-01 08:00:00」「7d」「24h」。
        默认会把近似重复的条目挪到后面（no_fold=True 关掉）；结果里 expanded 是按同义词组一起查了的写法。"""
        return self.call("search", {"q": q, "limit": limit, "kind": kind, "tag": tag, "topic_prefix": topic_prefix,
                                    "history": history, "explain": explain, "since": since, "until": until,
                                    "no_fold": no_fold})

    def aliases(self) -> list:
        """所有同义词组：[{"canon", "names"}]。"""
        return self.call("aliases").get("groups") or []

    def add_aliases(self, *names: str) -> dict:
        """把这些写法归为一组（「英伟达」「NVIDIA」「NVDA」），检索时一起查；和已有组重叠会合并。"""
        return self.call("aliases", body={"names": list(names)})

    def remove_alias(self, name: str) -> dict:
        return self.call("aliases", body={"name": name})

    def get(self, id: int) -> dict:
        return self.call("get", {"id": int(id)})

    def history(self, id: int = 0, topic: str = "") -> list:
        return self.call("history", {"id": int(id) if id else None, "topic": topic}).get("entries") or []

    def relate(self, id: int, limit: int = 8) -> list:
        return self.call("relate", {"id": int(id), "limit": limit}).get("entries") or []

    def due(self, days: int = 0, limit: int = 100) -> list:
        return self.call("due", {"days": days, "limit": limit}).get("entries") or []

    def stats(self) -> dict:
        return self.call("stats")
