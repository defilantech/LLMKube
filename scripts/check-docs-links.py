#!/usr/bin/env python3
"""Check the links and markup in docs/site before llmkube.com builds them.

llmkube.com renders every page docs/site/_meta/nav.yaml lists, with mdsvex,
and its build fails on a link to a page that does not exist. Those failures
surface only after a merge here, on the site's next build. This check runs the
same resolution in this repo's CI:

  - every link resolves the way the site resolves it, relative to the page's
    URL (/docs/<slug>), to a page nav.yaml lists; a link ending in .md is an
    error, because the site serves no .md files;
  - every #anchor exists on its page, using the site's heading-id rules;
  - every link to this repository's files on GitHub (blob/main, tree/main)
    names a path that exists, and its #anchor a heading in that file;
  - no text outside code is something mdsvex would read as markup: a line that
    starts with an unknown <tag>, a bare "<" (as in "<1s"), or a brace (a
    Svelte expression). The first breaks the page's structure; the others fail
    the build.

With --site <dir>, it also checks a built copy of the site (llmkube-web's
build/ directory): every internal link and #anchor on every page.

External links are not fetched.

Usage: scripts/check-docs-links.py [--site BUILD_DIR] [REPO_ROOT]

Exit codes:
  0 - no problems
  1 - problems found (each printed as file:line: message)
  2 - script error (missing nav.yaml, missing pyyaml)
"""

import argparse
import html
import os
import re
import sys
import urllib.parse

try:
    import yaml
except ImportError:
    print("ERROR: python3 pyyaml package required (pip3 install pyyaml)", file=sys.stderr)
    sys.exit(2)

GITHUB = re.compile(
    r"^https://github\.com/defilantech/llmkube/(?:blob|tree)/main/([^#?]*)(?:#(.*))?$", re.I
)
FENCE = re.compile(r"^\s*(`{3,}|~{3,})")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
# Elements and components a docs page may use. Anything else at the start of a
# line is a typo or a placeholder such as <name>, which mdsvex parses as an
# element and which then swallows the rest of the paragraph.
KNOWN_TAGS = {
    "a", "b", "br", "code", "details", "div", "em", "hr", "i", "img", "kbd", "li",
    "ol", "p", "pre", "span", "strong", "sub", "summary", "sup", "table", "tbody",
    "td", "th", "thead", "tr", "ul", "DocCallout", "AsciinemaCast",
}


def strip_fences(text):
    """Blank out fenced code blocks, keeping line numbers."""
    out, fence = [], None
    for line in text.split("\n"):
        m = FENCE.match(line)
        if m:
            if fence is None:
                fence = m.group(1)[0]
            elif m.group(1)[0] == fence:
                fence = None
            out.append("")
            continue
        out.append("" if fence else line)
    return "\n".join(out)


def strip_code_spans(text):
    """Blank out inline code, which may span a line break, keeping newlines."""
    return re.sub(r"(`+)(?:(?!\1).)+?\1", lambda m: re.sub(r"[^\n]", " ", m.group(0)), text, flags=re.S)


def heading_text(raw):
    t = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", raw)
    t = re.sub(r"<[^>]+>", "", t)
    t = t.replace("`", "").replace("*", "").replace("\\", "")
    return html.unescape(t)


def slugify(text):
    # The site's rehype-heading-ids rule: drop everything but letters, marks,
    # numbers, connector punctuation, spaces and hyphens; spaces become hyphens.
    # GitHub's heading anchors follow the same shape.
    t = "".join(c for c in text.strip().lower() if c.isalnum() or c in "_ -")
    return t.replace(" ", "-")


_anchors = {}


def anchors(path):
    """The heading ids a markdown file renders, with -1, -2 for repeats."""
    if path not in _anchors:
        ids, seen = set(), {}
        text = open(path, encoding="utf-8", errors="replace").read()
        for line in strip_fences(text).split("\n"):
            h = HEADING.match(line)
            if h:
                base = slugify(heading_text(h.group(2)))
                n = seen.get(base, 0)
                seen[base] = n + 1
                ids.add(base if n == 0 else f"{base}-{n}")
        ids.update(re.findall(r'\b(?:id|name)="([^"]+)"', text))
        _anchors[path] = ids
    return _anchors[path]


def load_nav(site):
    nav_path = os.path.join(site, "_meta", "nav.yaml")
    if not os.path.exists(nav_path):
        print(f"ERROR: {nav_path} not found", file=sys.stderr)
        sys.exit(2)
    pages = {}
    for section in yaml.safe_load(open(nav_path))["sections"]:
        for item in section["items"]:
            pages[item["slug"]] = item
    return pages


def page_markdown(site, item):
    """The markdown file a nav entry renders, or None for stubs and web pages."""
    if item.get("status") == "stub" or item.get("source") == "web" or not item.get("file"):
        return None
    path = os.path.join(site, item["file"])
    return path if os.path.exists(path) else None


def check_markup(rel, text, problems):
    fenced = strip_fences(text)
    # mdsvex reads a line that starts with <tag> as an element even when the
    # line continues an inline code span from the line above, so this check
    # runs before code spans are removed.
    for n, line in enumerate(fenced.split("\n"), 1):
        start = re.match(r"^\s*<(/?)([A-Za-z][\w.-]*)", line)
        if start and start.group(2) not in KNOWN_TAGS:
            problems.append(f"{rel}:{n}: <{start.group(2)}> starts a line, so mdsvex reads it as an element; reflow the paragraph so it does not")
    body = strip_code_spans(fenced)
    body = re.sub(r"<!--.*?-->", lambda m: re.sub(r"[^\n]", " ", m.group(0)), body, flags=re.S)
    in_component = False
    for n, line in enumerate(body.split("\n"), 1):
        for m in re.finditer(r"<(?!/?[A-Za-z]|!--)", line):
            problems.append(f"{rel}:{n}: bare '<' outside code is read as a tag; write 'under' or put it in code")
        for m in re.finditer(r"<([a-z][\w-]*)\b", line):
            if m.group(1) not in KNOWN_TAGS and not re.match(r"<(https?|mailto):", line[m.start():]):
                problems.append(f"{rel}:{n}: <{m.group(1)}> outside code is read as an element; put it in code")
        # Attribute values of the site's components ({[...]} props) are code.
        if re.search(r"<(DocCallout|AsciinemaCast)\b", line):
            in_component = True
        if not in_component and re.search(r"[{}]", line):
            problems.append(f"{rel}:{n}: brace outside code is a Svelte expression to mdsvex; put it in code")
        if in_component and re.search(r"/?>\s*$", line):
            in_component = False


def check_links(repo, site, pages, slug, path, problems):
    rel = os.path.relpath(path, repo)
    text = open(path, encoding="utf-8").read()
    check_markup(rel, text, problems)
    body = strip_code_spans(strip_fences(text))
    page_url = "https://llmkube.com/docs" + (f"/{slug}" if slug else "")
    for n, line in enumerate(body.split("\n"), 1):
        targets = re.findall(r"\]\(\s*<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\s*\)", line)
        targets += re.findall(r'\bhref="([^"]+)"', line)
        for raw in targets:
            where = f"{rel}:{n}: {raw}"
            if raw.startswith("#"):
                if raw[1:] and raw[1:] not in anchors(path):
                    problems.append(f"{where}: no heading #{raw[1:]} on this page")
                continue
            gh = GITHUB.match(raw)
            if gh:
                target_path = urllib.parse.unquote(gh.group(1)).rstrip("/")
                full = os.path.join(repo, target_path)
                if not os.path.exists(full):
                    problems.append(f"{where}: {target_path} does not exist on main")
                elif gh.group(2) and full.endswith(".md") and gh.group(2) not in anchors(full):
                    problems.append(f"{where}: no heading #{gh.group(2)} in {target_path}")
                continue
            if re.match(r"^[a-z][a-z0-9+.-]*:", raw) and not raw.startswith("https://llmkube.com/"):
                continue
            url = urllib.parse.urlparse(urllib.parse.urljoin(page_url, raw))
            if not url.path.startswith("/docs"):
                continue
            if url.path.endswith(".md"):
                problems.append(f"{where}: links to a .md file, which the site does not serve; drop the extension")
                continue
            target = url.path[len("/docs"):].strip("/")
            if target not in pages:
                problems.append(f"{where}: /docs/{target} is not in nav.yaml")
                continue
            frag = urllib.parse.unquote(url.fragment)
            target_md = page_markdown(site, pages[target])
            if frag and target_md and frag not in anchors(target_md):
                problems.append(f"{where}: no heading #{frag} on /docs/{target}")


def check_site(build, problems):
    ids = {}

    def page_ids(full):
        if full not in ids:
            ids[full] = set(re.findall(r'\bid="([^"]+)"', open(full, errors="replace").read()))
        return ids[full]

    def built_page(path):
        p = path.rstrip("/") or "/index"
        for cand in (p + ".html", p + "/index.html", p):
            full = os.path.join(build, cand.lstrip("/"))
            if os.path.isfile(full):
                return full
        return None

    for root, _, files in os.walk(build):
        for fn in files:
            if not fn.endswith(".html"):
                continue
            full = os.path.join(root, fn)
            url = "/" + os.path.relpath(full, build)[: -len(".html")]
            url = re.sub(r"/index$", "/", url)
            for href in sorted(set(re.findall(r'\bhref="([^"]+)"', open(full, errors="replace").read()))):
                href = re.sub(r"^https?://(www\.)?llmkube\.com", "", html.unescape(href)) or "/"
                if re.match(r"^[a-z][a-z0-9+.-]*:", href):
                    continue
                where = f"{url}: {href}"
                if href.startswith("#"):
                    if href[1:] and href[1:] not in page_ids(full):
                        problems.append(f"{where}: no element #{href[1:]} on this page")
                    continue
                u = urllib.parse.urlparse(urllib.parse.urljoin("https://llmkube.com" + url, href))
                if u.path.startswith("/_app/"):
                    continue
                if re.search(r"\.[a-z0-9]{2,12}$", u.path) and not u.path.endswith(".html"):
                    if not os.path.exists(os.path.join(build, u.path.lstrip("/"))):
                        problems.append(f"{where}: no file {u.path}")
                    continue
                target = built_page(u.path)
                if not target:
                    problems.append(f"{where}: no page {u.path}")
                elif u.fragment and urllib.parse.unquote(u.fragment) not in page_ids(target):
                    problems.append(f"{where}: no element #{u.fragment} on {u.path}")


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("repo", nargs="?", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
    parser.add_argument("--site", help="a built llmkube.com (llmkube-web's build/) to check as well")
    args = parser.parse_args()
    repo = os.path.abspath(args.repo)
    site = os.path.join(repo, "docs", "site")
    pages = load_nav(site)

    problems = []
    checked = 0
    for slug, item in pages.items():
        path = page_markdown(site, item)
        if path:
            check_links(repo, site, pages, slug, path, problems)
            checked += 1
    if args.site:
        check_site(os.path.abspath(args.site), problems)

    for p in problems:
        print(p)
    scope = f"{checked} docs/site pages" + (f" and the site built in {args.site}" if args.site else "")
    if problems:
        print(f"check-docs-links: {len(problems)} problem(s) in {scope}", file=sys.stderr)
        sys.exit(1)
    print(f"check-docs-links: {scope} OK")


if __name__ == "__main__":
    main()
