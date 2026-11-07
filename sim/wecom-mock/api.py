"""接收端 HTTP 接口（接口规范 §11.5）。

监听 ``127.0.0.1:8081``，不对外暴露（.claude/rules/sim.md）。

三条路由：

- ``POST /webhook``  —— Node-RED 的投递目标，也就是 ``WECOM_WEBHOOK_URL``
- ``GET /api/v1/records`` —— 记录查询，按接收时间倒序
- ``GET /`` —— 卡片页面，企业微信机器人消息的样子

**校验失败回 4xx 且不写记录**，与 Node-RED「参数校验失败不写投递日志」同一口径：
没有可信的 ``notifyId`` 做主键，强写会产生无主记录。这条在保存之前断，
不是在保存之后回滚。
"""

from __future__ import annotations

import time
from typing import Annotated, Any

from fastapi import FastAPI, Query
from fastapi.responses import HTMLResponse, JSONResponse

from card import Alarm, NotifyCard, render_card
from store import Record, RecordStore

DEFAULT_LIMIT = 50
MAX_LIMIT = 500

# 页面每 5 秒自刷新：接收端是演示窗口，手动刷新的那一刻往往正好没有新卡片，
# 看的人会以为通知没发出来。与 Grafana 面板同一刷新粒度。
REFRESH_SECONDS = 5


def now_ms() -> int:
    return int(time.time() * 1000)


def build_app(store: RecordStore) -> FastAPI:
    app = FastAPI(title="wecom-mock", docs_url=None, redoc_url=None)

    @app.post("/webhook")
    def webhook(card: NotifyCard) -> JSONResponse:
        record, duplicate = store.save(card, now_ms())
        return JSONResponse(
            status_code=200,
            content={
                "errcode": 0,
                "errmsg": "ok",
                "notifyId": record.notifyId,
                # 企业微信不带这两个字段，这里给出来只为让联调方一眼看出
                # 「重复投递被去重了」还是「真的又发了一张新卡」。
                "duplicate": duplicate,
                "retryCount": record.retryCount,
            },
        )

    @app.get("/api/v1/records")
    def records(
        limit: Annotated[int, Query(ge=1, le=MAX_LIMIT)] = DEFAULT_LIMIT,
    ) -> dict[str, Any]:
        items = [r.as_dict() for r in store.recent(limit)]
        return {"total": store.count(), "count": len(items), "records": items}

    @app.delete("/api/v1/records")
    def clear() -> dict[str, Any]:
        store.clear()
        return {"ok": True, "total": store.count()}

    @app.get("/", response_class=HTMLResponse)
    def page(limit: Annotated[int, Query(ge=1, le=MAX_LIMIT)] = DEFAULT_LIMIT) -> str:
        return render_page(store, limit)

    return app


def render_page(store: RecordStore, limit: int = DEFAULT_LIMIT) -> str:
    rows = store.recent(limit)
    cards = "".join(_render_item(r) for r in rows) or '<p class="empty">暂无投递记录</p>'
    return (
        '<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">'
        f'<meta http-equiv="refresh" content="{REFRESH_SECONDS}">'
        "<title>wecom-mock · 通知接收端</title>"
        f"<style>{_CSS}</style></head><body>"
        "<header><h1>通知接收端</h1>"
        f'<p class="meta">共 {store.count()} 条投递记录，显示最近 {len(rows)} 条，'
        f"每 {REFRESH_SECONDS} 秒刷新</p></header>"
        f"<main>{cards}</main></body></html>"
    )


def _render_item(record: Record) -> str:
    """一条记录渲染成卡片。卡片本体走 `card.render_card`，与单测同一条路径。"""
    alarm = Alarm(
        alarmId=record.alarmId,
        level=record.level,
        levelText=record.levelText,
        workshop=record.workshop,
        terminalId=record.terminalId,
        deviceText=record.deviceText,
        pointText=record.pointText,
        alarmType=record.alarmType,
        alarmTypeText=record.alarmTypeText,
        triggerValue=record.triggerValue,
        unit=record.unit,
        threshold=record.threshold,
        occurTime=record.occurTime,
        reason=record.reason,
        backfill=record.backfill,
    )
    retry = (
        f"<span>重复投递 {record.retryCount} 次</span>"
        if record.retryCount
        else "<span></span>"
    )
    return (
        '<article class="item">'
        f"{render_card(alarm)}"
        f'<div class="foot">{retry}'
        f"<span>{record.notifyId}</span>"
        f"<span>{record.channel}</span></div>"
        "</article>"
    )


_CSS = """
:root{--fg:#1f2329;--muted:#8f959e;--line:#e5e6eb;--bg:#f5f6f7}
*{box-sizing:border-box}
body{margin:0;padding:24px;background:var(--bg);color:var(--fg);
 font:14px/1.6 -apple-system,"PingFang SC","Microsoft YaHei",sans-serif}
header{max-width:720px;margin:0 auto 16px}
h1{margin:0;font-size:18px}
.meta{margin:4px 0 0;color:var(--muted);font-size:12px}
main{max-width:720px;margin:0 auto;display:flex;flex-direction:column;gap:12px}
.item{background:#fff;border-radius:8px;overflow:hidden;
 box-shadow:0 1px 2px rgba(0,0,0,.06)}
.card{padding:16px}
.card .head{display:flex;align-items:center;gap:8px;margin-bottom:8px}
.card .level{font-size:12px;padding:1px 8px;border-radius:10px;
 background:#e8f3ff;color:#1e6fff}
.level-1{background:#f2f3f5;color:#4e5969}
.level-2{background:#fff7e8;color:#d25f00}
.level-3{background:#ffece8;color:#d83931}
.level-4{background:#d83931;color:#fff}
.card .badge{font-size:12px;padding:1px 8px;border-radius:10px;
 background:#f2f3f5;color:var(--muted)}
.card .title{font-size:16px;font-weight:600;margin-bottom:10px}
.card .row{display:flex;gap:8px;padding:2px 0;color:#4e5969}
.card .k{flex:0 0 64px;color:var(--muted)}
.card .v{flex:1;word-break:break-all}
/* 补传的卡片整卡降透明度并去饱和：历史告警不能和实时告警同权重，
   但也不能藏起来——它确实发生过。 */
.card.is-backfill{opacity:.55;filter:grayscale(.7)}
.foot{display:flex;justify-content:space-between;gap:8px;padding:8px 16px;
 background:#fafafa;border-top:1px solid var(--line);
 color:var(--muted);font-size:12px;word-break:break-all}
.empty{color:var(--muted);text-align:center;padding:48px 0}
"""