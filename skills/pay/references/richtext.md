# Rich text as Markdown

Payload stores every `richText` field as Lexical JSON
(`{"root":{"type":"root","children":[…]}}`). Never build that JSON by hand and never parse
`children` with jq. `pay` converts both ways, locally, in the exact shape Payload 3.x
stores, and every write verb takes Markdown directly.

---

## 1. Write Markdown: `--set-md`

On `create`, `update`, `upsert`, `diff`, `globals update`, `upload`, and inside rows with
`pay blocks add|set|new`:

```bash
pay create posts --set title=Launch --set slug=launch --set-md content=@body.md --draft
pay update posts 3 --set-md content=@body.md --draft --dry-run
pay update pages 12 --set-md 'hero.richText=## Neu' --draft
pay upload media hero.png --alt Hero --set-md 'caption=Foto: **Team**'
pay globals update footer --set-md 'note=@footer.md' --dry-run
```

Rich text **inside a block**: set it on the row in the pipe; the key is relative to the
row (take `field` + `selector` from `pay outline`):

```bash
pay get pages 47 --draft --depth 0 \
  | pay blocks set id:6aba2c6c --set-md content=@card.md --field 'layout[id:6aba2c70].blocks' \
  | pay apply --draft --dry-run
pay get pages 47 --draft --depth 0 \
  | pay blocks add text --template --set heading=Ablauf --set-md content=@ablauf.md --after first \
  | pay apply --draft --dry-run
pay blocks new text --collection pages --set heading=Ablauf --set-md content=@ablauf.md
```

* Precedence: `--data`/`--data-file` → `--set` → `--set-json` → `--set-md` (later wins;
  the editor state replaces the field, never merges).
* Values: `@file.md`, `@-` (stdin; not in `pay blocks add|set` where stdin is the
  document, and not together with `--data @-` / `--data-file -`), or inline Markdown. Inline
  text starting with `@`: write `\@`. Empty stdin for `@-` is `no_input` (nothing is
  cleared); an empty `@file.md` warns `set_md_empty`; `KEY=` (empty inline) clears on purpose.
* Inline values need real line breaks: `--set-md 'content=# T\n\nText'` in single quotes
  is ONE heading reading `T\n\nText` (warning `set_md_literal_newline`). Use `@file.md`, a
  heredoc with `KEY=@-`, or `$'# T\n\nText'`.
* `pay update pages 47 --set-md layout.2.content=…` is refused (`invalid_args`): a write
  body replaces the whole `layout`. Use the `pay blocks set … --set-md` pipe.
* `--set-md title=…` on a text field is refused when the schema knows the type. When it
  does not (no introspection: every group sub-field such as `meta.description`), pay reads
  the stored value first: a stored string refuses the write; no stored value at all warns
  `set_md_field_unchecked` — check the key. In blocks, `set_md_not_richtext` warns when the
  key held a plain string or the census saw it as a scalar (`pay blocks schema <type>`:
  rich text is kind `richText`).
* `stored_as_string` after a write: an object you sent came back as JSON text — the field
  is a text field. Write the text with `--set KEY=…`.

---

## 2. Read rich text as Markdown or text: `--richtext`

```bash
pay get pages 47 --draft --richtext md            # every rich-text value (blocks too) → Markdown
pay get posts 3 --richtext text --path .content
pay find posts --limit 5 --select title,content --richtext md
pay globals get footer --richtext text
pay versions get pages 20 --richtext md --path .version.layout
```

`meta.richtext` = `{format, rendered, paths}` lists which strings were rich text. Default
is `json` (the stored Lexical JSON). Only lossy conversions warn (`lexical_lossy_*`).

**A `--richtext md|text` read is for reading only.** `pay apply`, every `pay blocks` edit
stage, `--data @-`/`--data-file`, `pay plan`/`sync` and `pay lexical to-md` refuse it
with `richtext_rendered` (exit 5) — Payload would otherwise STORE the Markdown string in
the richText field. A bare document (`--output raw`/`jsonl`, a jq extract) is refused too:
when a declared richText field holds a string, and — `update <id>`, `apply`, `globals
update`, `upsert`, `sync` — wherever the stored document holds rich text and the body a
string (blocks included; pay reads the document once to check). Only `update --where` and
`create` fall back to the census warning `richtext_as_string`. To edit: re-read without
`--richtext`.

---

## 3. Convert without writing: `pay lexical`

Local, no profile, no network.

```bash
pay lexical from-md body.md                         # Markdown → editor state (data)
pay lexical from-md body.md --as content            # → {"content": state}: a write body
pay lexical from-md body.md --output raw > body.lexical.json   # bare editor state, for a manifest
pay lexical to-md page.json --field content -o body.md         # editor state / doc / envelope → Markdown
pay lexical text page.json                          # → plain text, every rich-text field
```

```bash
# Markdown → a post's body (draft), one pipe
pay lexical from-md body.md --as content | pay update posts 12 --data @- --draft

# what does this page say? (every rich-text field, blocks included)
pay get pages 47 --draft --depth 0 | pay lexical text -o -
pay get pages 47 --draft --depth 0 | pay lexical to-md --path '.fields[].path'   # list the paths

# edit loop: to-md → edit the file → write back
pay get posts 3 --depth 0 --draft | pay lexical to-md --field content -o body.md
pay update posts 3 --set-md content=@body.md --draft --dry-run
```

* `to-md` / `text` read a bare state, a document, a `pay get` envelope or a `pay find`
  envelope, find EVERY rich-text value at any depth and return
  `data.fields[] = {path, markdown|text}`. `--field` picks one and accepts the
  `pay blocks` path grammar (`layout[2].richText`, `layout[id:…].blocks[type:grid].blocks[0].content`).
  An unknown `--field` is `not_rich_text` (exit 5) with the real paths in `did_you_mean`;
  a selector that matches nothing is `selector_no_match` (4).
* `-o -` prints the bare Markdown/text on stdout (the envelope goes to stderr).
  `--output raw --path .markdown` would print a JSON *string*, not raw Markdown.
* `from-md --as content` output is an envelope; `--data @-` unwraps it (an
  `envelope_unwrapped` warning is normal here). Dotted for groups: `--as meta.body`.
  A row path (`--as layout.0.content`) is refused: a write body replaces the whole array
  (Payload would empty it) — use `pay blocks set … --set-md` in a pipe.

---

## 4. The dialect

| Markdown | becomes |
|---|---|
| `**b**` `*i*` `~~s~~` `` `c` `` | bold / italic / strikethrough / code |
| `<u>u</u>` `<sub>x</sub>` `<sup>x</sup>` `<mark>x</mark>` | underline / sub / sup / highlight (HTML tags are the only spelling) |
| `# … ######` | heading h1–h6 |
| line ending in `\` (or two spaces, or `<br>`) | line break inside the paragraph; a single newline is a space |
| `[t](https://x)` / `[t](/path)` | custom link |
| `[t](pay:pages/47)` | **internal** link to pages 47 (follows slug changes) |
| `[t](pay:pages/slug=about)` | internal link to the page whose slug is `about`: a `{"$ref"}` create/update/upsert/sync resolve to the id (`pay lexical from-md` output keeps the `$ref`) |
| `[t](…){target=_blank}` | opens in a new tab |
| `[t][1]` + a line `[1]: https://x` | reference link → ordinary link |
| `<https://x>` | autolink |
| `- a` / `1. a` / `- [ ] a` `- [x] a` | bullet / numbered (start = first number) / check list; indent 2+ spaces to nest |
| `> q` | quote |
| `***` (also `---`, `___`) | horizontal rule (`to-md` writes `***`) |
| `![media:40]()` (own line) | upload node for media 40 |
| `<!-- lexical:block {"fields":{…}} -->` | any other node, verbatim (what `to-md` writes) |

Front matter (a leading `---` / YAML `key:` lines / `---` block) is stripped
(`frontmatter_stripped`, with its line count and first line; `--keep-frontmatter` on
`from-md` keeps it). Migration sources with YAML front matter can be passed as they are.
A file that starts with a rule followed by a blank line or prose is content, not front
matter.

---

## 5. Gotchas

* **Keep every `<!-- lexical:… -->` comment** that `to-md` / `--richtext md` wrote when you
  edit and write back: each one IS a node (a block, an upload with caption, a relationship,
  a table). Deleting the comment deletes the node. A placeholder whose JSON you broke fails
  the conversion (`bad_request_body`, exit 5) instead of silently dropping the node.
* Lost on the way to Markdown (`lexical_lossy_*` warnings): paragraph/heading alignment,
  indent, inline CSS colours, case transforms. Writing the Markdown back drops them.
* `lexical_link_url_relative`: links like `../brief.md` resolve against the PAGE URL on the
  site. Rewrite them to site paths or `pay:pages/ID` before writing.
* `lexical_link_url_invalid`: an empty link URL or one with a space. Drafts save;
  **publishing fails** with `link node failed to validate`.
* Not converted (warning + text kept): Markdown images other than `![media:ID]()` (upload
  the file first with `pay upload media file.png`, then use its id), pipe tables, raw
  HTML, fenced code blocks (become inline-code lines).
* A heading or quote inside a list item becomes the item's plain text
  (`lexical_list_item_block_flattened`; a quote's paragraphs are joined by line breaks, a
  list inside it stays a nested list).
* A bullet list with one `- [ ]` item becomes a check list: its plain items turn into
  unchecked checkboxes (`lexical_list_became_checklist`).
* A page's `layout` is blocks, not one rich-text field: convert each block's own rich text,
  don't pour a whole page into one field.
* Round trip keeps the content of unchanged text (`--richtext md` → edit → `--set-md`),
  except the lossy items above — but not byte for byte: the write normalises bookkeeping
  keys once (`direction`, paragraph `textFormat`/`textStyle`, link `id`), so `pay diff` /
  `upsert` report a `formatting` change for a field written back unchanged. Write back only
  the fields you edited.
* `pay diff` shows rich text as text with placeholders (`[upload media:12]`,
  `[block cta]`); `--full` shows the editor JSON.
