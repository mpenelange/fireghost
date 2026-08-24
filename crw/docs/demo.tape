# VHS Demo - fastCRW
# Mirrors the README CLI tour: URL -> markdown -> JSON -> search ->
# AI summary -> the agent loop -> structured extraction.
#
# Steps 4-6 use an LLM. Export DEEPSEEK_API_KEY in your shell before
# running `vhs docs/demo.tape` — the hidden setup block below reads it
# from the environment so the key is never written into this file.
Output docs/demo.gif

Set Shell zsh
Set FontSize 13
Set Width 1100
Set Height 1150
Set Theme "Catppuccin Mocha"
Set Padding 20
Set TypingSpeed 50ms

# Setup (hidden): PATH + LLM provider via env vars.
# $DEEPSEEK_API_KEY is expanded by the real shell from vhs's environment.
Hide
Type "export PATH=/Users/us/coding/crw/crw-opencore/target/release:$PATH"
Enter
Type "export CRW_EXTRACTION__LLM__PROVIDER=openai CRW_EXTRACTION__LLM__MODEL=deepseek-chat CRW_EXTRACTION__LLM__BASE_URL=https://api.deepseek.com CRW_EXTRACTION__LLM__API_KEY=$DEEPSEEK_API_KEY"
Enter
Type "clear"
Enter
Show
Sleep 500ms

# 1. Any page -> clean, LLM-ready markdown
Type "crw en.wikipedia.org/wiki/Web_scraping | head -25"
Enter
Sleep 4500ms

# 2. Structured JSON for pipelines
Type "crw en.wikipedia.org/wiki/Web_scraping --format json | jq '.metadata'"
Enter
Sleep 4500ms

# 3. Search the web
Type "crw search 'rust async runtime tokio' | head -16"
Enter
Sleep 5500ms

# (hidden) clear so each AI result gets a clean screen
Hide
Type "clear"
Enter
Show
Sleep 500ms

# 4. Scrape + AI summary
Type "crw en.wikipedia.org/wiki/Web_scraping --summary --prompt 'in 3 bullet points'"
Enter
Sleep 12000ms

# 5. The agent loop: search -> scrape -> summarize
Type `crw "$(crw search 'what is retrieval augmented generation' --format json | jq -r '.[0].url')" --summary --prompt 'Explain it to a backend engineer in 4 sentences.'`
Enter
Sleep 15000ms

# (hidden) clear before the structured-extraction finale
Hide
Type "clear"
Enter
Show
Sleep 500ms

# 6. Structured extraction with a JSON Schema
Type `crw news.ycombinator.com --extract '{"type":"object","properties":{"stories":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"points":{"type":"integer"}}}}}}' | jq '.stories[:5]'`
Enter
Sleep 14000ms

Sleep 1500ms
