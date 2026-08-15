#!/usr/bin/env python3
"""
sync_pricing.py — 从 models.dev 仓库同步官方模型定价到 internal/llm/pricing.go

用法:
    python3 scripts/sync_pricing.py [--diff] [--write]

    --diff   只对比差异，不写文件（默认）
    --write  将生成的定价表代码写回 internal/llm/pricing.go 的定价表区间

数据源: https://github.com/sst/models.dev (providers/<厂商>/models/*.toml 的 [cost] 段)
官方 API 价基准（非 OpenCode Go 套餐价）。

原理:
    1. 下载 models.dev 仓库 tarball（走 ghproxy 镜像，直连 github 可能超时）
    2. 解析 providers/{zhipuai,deepseek,moonshotai,minimax,alibaba,xai,
       openai,anthropic,google}/models/*.toml 的 [cost] 段
    3. 生成 Go map 条目（key=模型文件名小写，前缀边界匹配语义）
    4. 与 pricing.go 当前 pricingTable 对比，输出差异报告

注意:
    - 定价 key 使用模型文件名（小写）。pricing.go 的 LookupPricing 按
      '-' 边界做前缀匹配，因此 "gpt-5.2" 会命中 "gpt-5.2-pro"——
      价格差异大的变体（如 2x 高速版）需在生成后手工补更长的 key。
    - 生成结果需人工审查后提交（models.dev 是社区数据源，价格可能有误）。
"""

import io
import json
import os
import re
import subprocess
import sys
import tarfile
import urllib.request

REPO = "https://ghproxy.net/https://github.com/sst/models.dev/archive/refs/heads/master.tar.gz"
# 支持的厂商目录（models.dev 的 provider slug）
PROVIDERS = [
    "zhipuai", "deepseek", "moonshotai", "minimax", "alibaba", "xai",
    "openai", "anthropic", "google",
]

# 模型文件名 → 定价 key 的重命名（官方文件名与 API 模型名不一致时用）
RENAME = {
    # MiniMax 官方模型名带前缀（MiniMax-M2.7），API 里的名字就是 MiniMax-M2.7，
    # 小写化后即 minimax-m2.7，无需重命名。
}

# 需要跳过的模型（无实价、占位符、或非 chat 类模型）
SKIP = {
    "zhipuai": {"glm-4.5-flash", "glm-4.7-flash", "glm-4.7-flashx"},
    "alibaba": set(),  # 阿里云 54 个模型太多，脚本只导出白名单（见 MODEL_WHITELIST）
    "google": set(),   # Google 只导白名单
    "openai": set(),   # OpenAI 只导白名单
}

# 白名单：只导出这些厂商的这些模型（前缀匹配；None = 全部）
MODEL_WHITELIST = {
    # 阿里云 54 个模型，只保留旗舰 chat 系列（其余由前缀匹配命中的概率低）
    "alibaba": [
        "qwen3.7-max", "qwen3.7-plus", "qwen3.8-max", "qwen3.6-plus",
        "qwen3.6-max-preview", "qwen3-max", "qwen-plus", "qwen-max",
        "qwen-turbo", "qwq-plus", "qvq-max",
    ],
    # Google 34 个模型，只保留 chat 系列
    "google": [
        "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-pro",
        "gemini-3-flash-preview", "gemini-3.1-flash-lite", "gemini-3.1-pro-preview",
        "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash",
        "gemini-3.7-flash",
    ],
    # OpenAI 43 个模型，只保留主流 chat 系列
    "openai": [
        "gpt-3.5-turbo", "gpt-4", "gpt-4-turbo", "gpt-4o", "gpt-4o-mini",
        "gpt-4.1", "gpt-4.1-mini", "gpt-4.1-nano",
        "gpt-5", "gpt-5-mini", "gpt-5-nano", "gpt-5-pro",
        "gpt-5.1", "gpt-5.2", "gpt-5.2-pro", "gpt-5.3-codex",
        "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.5", "gpt-5.6",
        "gpt-5.6-luna", "o1", "o1-mini", "o3", "o3-mini", "o4-mini",
    ],
    # Anthropic 13 个模型全导
    "anthropic": None,
    # 其余厂商全导
    "deepseek": None,
    "moonshotai": None,
    "minimax": None,
    "xai": None,
    "zhipuai": None,
}

# 定价表注释头（生成代码的说明）
HEADER = """// pricingTable 内置定价表。key 是模型名前缀（如 "gpt-4o" 匹配 "gpt-4o-2024-..."）。
// 查找时按 key 长度降序，优先匹配更具体的前缀；前缀必须落在模型名边界上
// （key 之后是 '-' 或结束），避免 "gpt-4" 误匹配 "gpt-4o"。
// 数据来源：models.dev 官方 API 价（%s 同步，scripts/sync_pricing.py 生成）。
"""


def download_repo(dest):
    """下载并解压 models.dev 仓库到 dest，返回根目录路径。"""
    print(f"下载 {REPO} ...", file=sys.stderr)
    req = urllib.request.Request(REPO, headers={"User-Agent": "packetlab-sync"})
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = resp.read()
    print(f"下载完成 {len(data)} 字节，解压中 ...", file=sys.stderr)
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tf:
        names = [n for n in tf.getnames() if not n.startswith(("/", "../"))]
        tf.extractall(dest)
    root = os.path.join(dest, os.path.commonprefix([n for n in names if n]))
    return root.rstrip("/")


def parse_section(text, section):
    """解析 TOML 命名段 [xxx] 的 key=value（到下一个段为止）。"""
    out = {}
    lines = text.split("\n")
    i = 0
    target = f"[{section}]"
    while i < len(lines) and lines[i].strip() != target:
        i += 1
    if i >= len(lines):
        return out
    i += 1
    while i < len(lines):
        line = lines[i].strip()
        if line.startswith("["):
            break
        if "=" in line and not line.startswith("#"):
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip()
        i += 1
    return out


def parse_limit(text):
    """解析 [limit] 段（context / output），返回 {context, output} 整数。"""
    d = parse_section(text, "limit")
    out = {}
    for k in ("context", "output"):
        v = d.get(k)
        if v is None:
            continue
        v = v.split("#")[0].strip()  # 去行内注释
        try:
            out[k] = int(float(v.replace("_", "")))
        except (ValueError, TypeError):
            pass
    return out


def to_float(v):
    try:
        return float(str(v).replace("_", ""))
    except (ValueError, TypeError):
        return None


def collect_pricing(root):
    """遍历厂商目录收集 (provider, model, cost, limit) 元组。"""
    rows = []
    for prov in PROVIDERS:
        p = os.path.join(root, "providers", prov, "models")
        if not os.path.isdir(p):
            print(f"  {prov}: 目录不存在，跳过", file=sys.stderr)
            continue
        whitelist = MODEL_WHITELIST.get(prov)
        for f in sorted(os.listdir(p)):
            if not f.endswith(".toml"):
                continue
            name = f[:-5]
            if whitelist is not None and name not in whitelist:
                continue
            try:
                text = open(os.path.join(p, f)).read()
            except OSError:
                continue  # 断链 symlink
            cost = parse_section(text, "cost")
            inp = to_float(cost.get("input"))
            out = to_float(cost.get("output"))
            cache = to_float(cost.get("cache_read"))
            if inp is None or out is None or inp <= 0 or out <= 0:
                continue  # 无实价
            limit = parse_limit(text)
            rows.append((prov, name, inp, out, cache, limit))
    return rows


def go_literal(rows):
    """生成 Go map 字面量。按 key 排序；含 cache_read 时带第三字段。"""
    lines = []
    for prov, name, inp, out, cache in sorted(rows, key=lambda r: r[1].lower()):
        key = name.lower()
        inp_s = f"{inp:g}"
        out_s = f"{out:g}"
        if cache is not None and cache > 0:
            cache_s = f"{cache:g}"
            lines.append(f'\t"{key}": {{InputPerMTokens: {inp_s}, OutputPerMTokens: {out_s}, CacheReadPerMTokens: {cache_s}}},')
        else:
            lines.append(f'\t"{key}": {{InputPerMTokens: {inp_s}, OutputPerMTokens: {out_s}}},')
    return "\n".join(lines)


def read_current_table(path):
    """读取 pricing.go 中当前定价表 map（key → 行文本）。"""
    src = open(path).read()
    start = src.index("var pricingTable = map[string]ModelPricing{")
    # 找 map 结束的 "}"（在 start 之后第一个顶格 "}"）
    brace_end = src.index("\n}", start)
    body = src[start:brace_end + 2]
    current = {}
    for line in body.split("\n"):
        km = re.match(r'\s*"([^"]+)"', line)
        if km:
            current[km.group(1)] = line.strip()
    return current, (start, brace_end + 2)


def main():
    args = sys.argv[1:]
    do_write = "--write" in args
    # merge 模式：保留现有表中、models.dev 已下架的旧模型 key（只更新重叠项 + 追加新模型）
    # 不 merge 时全量覆盖（会删除旧 key）。默认 merge，历史模型定价留着无害。
    merge = "--no-merge" not in args

    tmp = "/tmp/models.dev-sync"
    os.makedirs(tmp, exist_ok=True)
    root = download_repo(tmp)

    rows = collect_pricing(root)
    print(f"\n收集到 {len(rows)} 个带价模型", file=sys.stderr)

    # 定价表 key 去重（不同厂商同名模型时，保留先出现的；这里全部厂商共享一个表）
    seen = {}
    limits_seen = {}
    for prov, name, inp, out, cache, limit in rows:
        key = name.lower()
        if key not in seen:
            seen[key] = (prov, name, inp, out, cache)
        if key not in limits_seen:
            limits_seen[key] = limit

    pricing_path = os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..", "internal", "llm", "pricing.go"
    )
    pricing_path = os.path.normpath(pricing_path)
    limits_path = os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..", "internal", "llm", "limits.go"
    )
    limits_path = os.path.normpath(limits_path)

    current, span = read_current_table(pricing_path)

    # merge：把 current 中未出现在 models.dev 的旧 key 补回 rows
    if merge:
        old_re = re.compile(r'"([^"]+)": \{(.*?)\}') 
        for key, line in current.items():
            if key in seen:
                continue
            m = re.search(r"InputPerMTokens: ([\d.]+), OutputPerMTokens: ([\d.]+)(?:, CacheReadPerMTokens: ([\d.]+))?", line)
            if not m:
                continue
            inp, out = float(m.group(1)), float(m.group(2))
            cache = float(m.group(3)) if m.group(3) else None
            seen[key] = ("legacy", key, inp, out, cache)

    new_rows = sorted(seen.values(), key=lambda r: r[1].lower())
    new_literal = go_literal(new_rows)

    # diff 报告：只报新增（相对当前表）和价格变化，删除只提示不执行（merge 模式保留）
    new_map = {r[1].lower(): r for r in new_rows}
    print("=== 新增（相对当前表）===")
    for key in sorted(set(new_map) - set(current)):
        _, name, i, o, c = new_map[key]
        print(f"  + {key:30} {i:>7}/{o:<7}" + (f" cr={c:g}" if c else ""))
    print("=== 价格变化 ===")
    for key in sorted(set(current) & set(new_map)):
        _, name, i, o, c = new_map[key]
        old = current[key]
        om = re.search(r"InputPerMTokens: ([\d.]+), OutputPerMTokens: ([\d.]+)", old)
        if om:
            oi, oo = float(om.group(1)), float(om.group(2))
            if abs(oi - i) > 1e-9 or abs(oo - o) > 1e-9:
                print(f"  ~ {key:30} {oi:>7}/{oo:<7} → {i:>7}/{o:<7}")
    print("=== 仅存于当前表（merge 保留；--no-merge 会删除）===")
    for key in sorted(set(current) - set(new_map)):
        print(f"  - {key}")

    if do_write:
        import datetime
        header = HEADER % datetime.date.today().isoformat()
        new_block = f"var pricingTable = map[string]ModelPricing{{\n{new_literal}\n}}"
        src = open(pricing_path).read()
        new_src = src[:span[0]] + new_block + src[span[1]:]
        open(pricing_path, "w").write(new_src)
        print(f"\n已写入 {pricing_path}（{'merge' if merge else '全量覆盖'} 模式）")
        # 生成 limits.go（context/output 限制表）
        limits_literal = generate_limits_go(limits_seen)
        open(limits_path, "w").write(limits_literal)
        print(f"已写入 {limits_path}（{len(limits_seen)} 个模型限制）")
        print("下一步：go build ./... && go test ./internal/llm/ 验证")
    else:
        print("\n（--diff 模式，未写文件。确认差异后加 --write 重新运行）")


def generate_limits_go(limits):
    """生成 limits.go 文件内容（模型上下文/输出限制表 + 查询函数）。"""
    lines = [
        "package llm",
        "",
        "// limits.go — 模型上下文/输出限制（models.dev [limit] 段，scripts/sync_pricing.py 生成）。",
        "// 未收录的模型返回零值（前端不展示限制）。",
        "",
        "// ModelLimits 模型上下文与输出长度限制（tokens）。",
        "type ModelLimits struct {",
        "\tContextLength int // 最大上下文长度",
        "\tMaxOutput     int // 最大输出长度",
        "}",
        "",
        "// modelLimits 限制表。key 语义同 pricingTable（'-' 边界前缀匹配）。",
        "var modelLimits = map[string]ModelLimits{",
    ]
    for key in sorted(limits):
        lim = limits[key]
        ctx = lim.get("context", 0)
        out = lim.get("output", 0)
        if ctx <= 0 and out <= 0:
            continue
        lines.append(f'\t"{key}": {{ContextLength: {ctx}, MaxOutput: {out}}},')
    lines += [
        "}",
        "",
        "// LookupLimits 查找模型限制。与 LookupPricing 相同的前缀边界匹配语义。",
        "// 未收录返回零值（ContextLength 与 MaxOutput 均为 0）。",
        "func LookupLimits(model string) ModelLimits {",
        "\tm := toLowerASCII(model)",
        "\tvar best ModelLimits",
        "\tvar bestKeyLen int",
        "\tfor key, lim := range modelLimits {",
        "\t\tk := toLowerASCII(key)",
        "\t\tif len(k) <= bestKeyLen {",
        "\t\t\tcontinue",
        "\t\t}",
        "\t\tif !hasPrefix(m, k) {",
        "\t\t\tcontinue",
        "\t\t}",
        "\t\tif len(m) > len(k) && m[len(k)] != '-' {",
        "\t\t\tcontinue",
        "\t\t}",
        "\t\tbest = lim",
        "\t\tbestKeyLen = len(k)",
        "\t}",
        "\treturn best",
        "}",
        "",
    ]
    return "\n".join(lines)


if __name__ == "__main__":
    main()
