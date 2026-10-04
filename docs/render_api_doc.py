#!/usr/bin/env python3
"""把 docs/API.md 渲染成单文件 HTML（侧栏目录 + 搜索高亮 + 回到顶部）。

用法：
    pip install markdown
    python docs/render_api_doc.py

约定：
- 只依赖 `markdown`，产物是自包含单文件（无外链、无 CDN），可直接双击打开或丢给同事。
- 侧栏目录由 markdown 的 toc 扩展生成（`toc_depth: 2-3`），锚点用下面的 slugify
  生成，必须和 API.md 里手写目录的链接保持一致 —— 中文标题要做 NFKC 归一化，
  否则「：」「（）」等全角标点会留在锚点里导致跳转失效。
- 正文里手写的「## 目录」列表只从 HTML 渲染输入里剔除（Markdown 文件保持原样），
  避免和侧栏目录重复。
- API.md 改动后重跑本脚本即可刷新 API.html。
"""
import pathlib
import re
import unicodedata

import markdown

# 脚本在 docs/ 下，仓库根目录是它的上一级
ROOT = pathlib.Path(__file__).resolve().parent.parent
MD = ROOT / "docs/API.md"
OUT = ROOT / "docs/API.html"

source = MD.read_text(encoding="utf-8")

# HTML 版左侧已有完整侧栏目录，正文里手写的「## 目录」列表是重复噪音 —— 仅从渲染输入里剔除，
# docs/API.md 保持不变（Markdown 阅读时仍需要它）。
source = re.sub(
    r"^##\s*目录\s*$\n(?:.*\n)*?^---\s*$",
    "",
    source,
    flags=re.M,
)


def slugify(value, separator="-"):
    """保留中文的 slug，供 toc 扩展生成锚点，与文档里的手写目录链接保持一致。"""
    value = unicodedata.normalize("NFKC", value).strip().lower()
    value = re.sub(r"[^\w\s\u4e00-\u9fff-]", "", value, flags=re.UNICODE)
    value = re.sub(r"\s+", separator, value)
    return value or "section"


md = markdown.Markdown(
    extensions=["tables", "fenced_code", "toc", "sane_lists", "attr_list"],
    extension_configs={
        "toc": {"slugify": slugify, "permalink": False, "toc_depth": "2-3"},
    },
)
body = md.convert(source)

# toc 扩展把目录塞进 md.toc，直接用它替换正文里手写的目录列表更稳；这里改用手写目录 + 独立侧栏
toc_html = md.toc

# 统计
section_count = len(re.findall(r"<h3", body))
table_count = len(re.findall(r"<table>", body))
chapter_count = len(re.findall(r"<h2", body))

TEMPLATE = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>comfyui-server 接口文档</title>
<style>
:root {
  --bg: #ffffff;
  --bg-side: #f7f8fa;
  --bg-code: #f5f6f8;
  --bg-hover: #eef0f4;
  --text: #1a1d21;
  --text-2: #5c636e;
  --text-3: #8b919c;
  --border: #e3e6ea;
  --accent: #2f6feb;
  --accent-bg: #eaf1fe;
  --get: #1d9e75;
  --post: #2f6feb;
  --put: #ba7517;
  --patch: #7f77dd;
  --del: #d85a30;
  --radius: 8px;
}
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }
body {
  background: var(--bg);
  color: var(--text);
  font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Hiragino Sans GB",
               "Microsoft YaHei", "Segoe UI", Roboto, sans-serif;
  font-size: 14px;
  line-height: 1.7;
  display: flex;
}
#sidebar {
  width: 290px;
  flex: 0 0 290px;
  height: 100vh;
  position: sticky;
  top: 0;
  overflow-y: auto;
  background: var(--bg-side);
  border-right: 1px solid var(--border);
  padding: 20px 0 40px;
}
#sidebar .brand {
  padding: 0 20px 14px;
  border-bottom: 1px solid var(--border);
  margin-bottom: 12px;
}
#sidebar .brand h1 {
  font-size: 15px;
  font-weight: 600;
  margin: 0 0 4px;
  letter-spacing: .2px;
}
#sidebar .brand p {
  margin: 0;
  font-size: 11.5px;
  color: var(--text-3);
  line-height: 1.5;
}
#search-wrap { padding: 0 16px 12px; }
#search {
  width: 100%;
  padding: 7px 10px;
  font-size: 13px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: #fff;
  color: var(--text);
  outline: none;
  font-family: inherit;
}
#search:focus { border-color: var(--accent); }
#search-info {
  font-size: 11.5px;
  color: var(--text-3);
  padding: 6px 20px 0;
  min-height: 16px;
}
#toc { list-style: none; margin: 0; padding: 0; }
#toc ul { list-style: none; margin: 0; padding: 0; }
#toc .toc > ul > li > a {
  display: block;
  padding: 7px 20px;
  color: var(--text-2);
  text-decoration: none;
  font-size: 13px;
  font-weight: 500;
  border-left: 2px solid transparent;
}
#toc .toc > ul > li > a:hover { background: var(--bg-hover); color: var(--text); }
#toc .toc > ul > li > a.active {
  color: var(--accent);
  background: var(--accent-bg);
  border-left-color: var(--accent);
}
#toc .toc ul ul a {
  display: block;
  padding: 5px 20px 5px 34px;
  color: var(--text-3);
  text-decoration: none;
  font-size: 12.5px;
  border-left: 2px solid transparent;
}
#toc .toc ul ul a:hover { color: var(--text); background: var(--bg-hover); }
#toc .toc ul ul a.active {
  color: var(--accent);
  background: var(--accent-bg);
  border-left-color: var(--accent);
}
main {
  flex: 1 1 auto;
  min-width: 0;
  padding: 36px 52px 120px;
  max-width: 1180px;
}
main > h1 {
  font-size: 26px;
  font-weight: 600;
  margin: 0 0 6px;
  padding-bottom: 14px;
  border-bottom: 2px solid var(--border);
}
h1 { font-size: 26px; font-weight: 600; }
h2 {
  font-size: 20px;
  font-weight: 600;
  margin: 46px 0 14px;
  padding: 8px 0 8px 12px;
  border-left: 3px solid var(--accent);
  background: linear-gradient(90deg, var(--accent-bg), transparent 60%);
  scroll-margin-top: 16px;
}
h3 {
  font-size: 16px;
  font-weight: 600;
  margin: 32px 0 10px;
  scroll-margin-top: 16px;
}
h4 { font-size: 14px; font-weight: 600; margin: 22px 0 8px; }
p { margin: 10px 0; }
a { color: var(--accent); }
code {
  font-family: "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
  font-size: 12.5px;
  background: var(--bg-code);
  padding: 2px 5px;
  border-radius: 4px;
  color: #b5306b;
}
pre {
  position: relative;
  background: #f7f8fa;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 14px 16px;
  overflow-x: auto;
  margin: 12px 0;
  line-height: 1.55;
}
pre code {
  background: none;
  padding: 0;
  color: #24292f;
  font-size: 12.5px;
}
table {
  border-collapse: collapse;
  width: 100%;
  margin: 12px 0 18px;
  font-size: 13px;
  display: block;
  overflow-x: auto;
}
th, td {
  border: 1px solid var(--border);
  padding: 7px 11px;
  text-align: left;
  vertical-align: top;
}
th {
  background: var(--bg-side);
  font-weight: 600;
  white-space: nowrap;
  color: var(--text-2);
  font-size: 12.5px;
}
tbody tr:nth-child(even) { background: #fcfcfd; }
tbody tr:hover { background: var(--bg-hover); }
td code { white-space: nowrap; }
blockquote {
  margin: 12px 0;
  padding: 10px 16px;
  background: #fffbeb;
  border-left: 3px solid #ef9f27;
  border-radius: 0 var(--radius) var(--radius) 0;
  color: #5b4a1f;
  font-size: 13px;
}
blockquote p { margin: 4px 0; }
blockquote code { background: #fdf3d7; color: #8a5a00; }
ul, ol { padding-left: 24px; margin: 10px 0; }
li { margin: 5px 0; }
hr { border: none; border-top: 1px solid var(--border); margin: 34px 0; }
/* 方法徽章：给总览表里的方法列上色 */
.done { color: var(--get); font-weight: 600; }
.hidden { display: none !important; }
mark { background: #fff3a3; color: inherit; padding: 0 2px; border-radius: 2px; }
#top-btn {
  position: fixed;
  right: 26px;
  bottom: 26px;
  width: 38px;
  height: 38px;
  border-radius: 50%;
  border: 1px solid var(--border);
  background: #fff;
  color: var(--text-2);
  font-size: 16px;
  cursor: pointer;
  display: none;
  box-shadow: 0 2px 8px rgba(0,0,0,.08);
}
#top-btn:hover { color: var(--accent); border-color: var(--accent); }
@media (max-width: 900px) {
  #sidebar { display: none; }
  main { padding: 20px 18px 80px; }
}
</style>
</head>
<body>
<nav id="sidebar">
  <div class="brand">
    <h1>comfyui-server 接口文档</h1>
    <p>__CHAPTER__ 章 · __SECTION__ 节 · 54 个接口 · __TABLE__ 张表</p>
  </div>
  <div id="search-wrap">
    <input id="search" type="search" placeholder="搜索接口 / 字段…" autocomplete="off">
  </div>
  <div id="search-info"></div>
  <div id="toc">__TOC__</div>
</nav>
<main id="content">
__BODY__
</main>
<button id="top-btn" title="回到顶部">↑</button>
<script>
(function () {
  var content = document.getElementById('content');
  var toc = document.getElementById('toc');
  var search = document.getElementById('search');
  var info = document.getElementById('search-info');
  var headings = Array.prototype.slice.call(content.querySelectorAll('h2, h3'));

  headings.forEach(function (h) {
    h.id = h.id || h.textContent.trim();
  });

  // 滚动高亮当前小节
  var links = {};
  toc.querySelectorAll('a').forEach(function (a) {
    links[decodeURIComponent(a.getAttribute('href').slice(1))] = a;
  });
  var observer = new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (!e.isIntersecting) return;
      toc.querySelectorAll('a.active').forEach(function (a) { a.classList.remove('active'); });
      var a = links[e.target.id];
      if (a) {
        a.classList.add('active');
        var rect = a.getBoundingClientRect();
        if (rect.top < 0 || rect.bottom > window.innerHeight) {
          a.scrollIntoView({ block: 'center' });
        }
      }
    });
  }, { rootMargin: '-10% 0px -75% 0px' });
  headings.forEach(function (h) { observer.observe(h); });

  // 目录过滤 + 正文命中高亮
  var marks = [];
  function clearMarks() {
    marks.forEach(function (m) {
      var p = m.parentNode;
      if (!p) return;
      p.replaceChild(document.createTextNode(m.textContent), m);
      p.normalize();
    });
    marks = [];
  }
  function highlight(q) {
    // 在 content 的文本节点里就地包一层高亮标签，跳过 script/style 与已高亮的节点
    var walker = document.createTreeWalker(content, NodeFilter.SHOW_TEXT, {
      acceptNode: function (node) {
        if (!node.nodeValue || !node.nodeValue.trim()) return NodeFilter.FILTER_REJECT;
        var p = node.parentNode;
        if (!p) return NodeFilter.FILTER_REJECT;
        var tag = p.nodeName;
        if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'MARK') return NodeFilter.FILTER_REJECT;
        return node.nodeValue.toLowerCase().indexOf(q) !== -1
          ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT;
      }
    });
    var targets = [];
    while (walker.nextNode()) targets.push(walker.currentNode);
    targets.forEach(function (node) {
      var text = node.nodeValue, lower = text.toLowerCase(), frag = document.createDocumentFragment();
      var from = 0, at;
      while ((at = lower.indexOf(q, from)) !== -1) {
        if (at > from) frag.appendChild(document.createTextNode(text.slice(from, at)));
        var m = document.createElement('mark');
        m.textContent = text.slice(at, at + q.length);
        frag.appendChild(m);
        marks.push(m);
        from = at + q.length;
      }
      if (from < text.length) frag.appendChild(document.createTextNode(text.slice(from)));
      node.parentNode.replaceChild(frag, node);
    });
    return marks.length;
  }
  search.addEventListener('input', function () {
    var q = search.value.trim().toLowerCase();
    var tocLis = Array.prototype.slice.call(toc.querySelectorAll('li'));
    clearMarks();
    if (!q) {
      tocLis.forEach(function (li) { li.classList.remove('hidden'); });
      info.textContent = '';
      return;
    }
    var headingHits = tocLis.filter(function (li) {
      var a = li.querySelector('a');
      return !!(a && a.textContent.toLowerCase().indexOf(q) !== -1);
    });
    // 标题一条都没命中时不要把整个目录清空 —— 那看起来像导航坏了；
    // 保留全量目录，只在信息栏说明「标题未命中」，用户仍可凭正文高亮定位。
    if (headingHits.length) {
      tocLis.forEach(function (li) {
        li.classList.toggle('hidden', headingHits.indexOf(li) === -1);
      });
    } else {
      tocLis.forEach(function (li) { li.classList.remove('hidden'); });
    }
    var bodyHits = highlight(q);
    info.textContent = headingHits.length
      ? ('目录 ' + headingHits.length + ' 节 · 正文 ' + bodyHits + ' 处')
      : ('标题未命中 · 正文 ' + bodyHits + ' 处');
    var first = content.querySelector('mark');
    if (first) first.scrollIntoView({ block: 'center', behavior: 'smooth' });
  });

  // 表格首列方法徽章上色
  content.querySelectorAll('table').forEach(function (t) {
    t.querySelectorAll('tbody tr').forEach(function (tr) {
      var cell = tr.children[0];
      if (!cell) return;
      var v = cell.textContent.trim();
      var colors = { GET: 'var(--get)', POST: 'var(--post)', PUT: 'var(--put)', PATCH: 'var(--patch)', DELETE: 'var(--del)' };
      if (colors[v]) { cell.style.color = colors[v]; cell.style.fontWeight = '600'; }
    });
  });

  var topBtn = document.getElementById('top-btn');
  window.addEventListener('scroll', function () {
    topBtn.style.display = window.scrollY > 500 ? 'block' : 'none';
  });
  topBtn.addEventListener('click', function () { window.scrollTo({ top: 0, behavior: 'smooth' }); });
})();
</script>
</body>
</html>
"""

html_out = (
    TEMPLATE.replace("__BODY__", body)
    .replace("__TOC__", toc_html)
    .replace("__CHAPTER__", str(chapter_count))
    .replace("__SECTION__", str(section_count))
    .replace("__TABLE__", str(table_count))
)
OUT.write_text(html_out, encoding="utf-8")
print(f"已生成 {OUT}")
print(
    f"大小 {OUT.stat().st_size / 1024:.1f} KB，章 {chapter_count}，节 {section_count}，表格 {table_count}"
)
