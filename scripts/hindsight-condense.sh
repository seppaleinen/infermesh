#!/usr/bin/env bash
# hindsight-condense.sh — find overlapping Hindsight pages and suggest merges
# Uses the Hindsight REST API directly (works outside opencode tool context)

set -euo pipefail

THRESHOLD=75
FULL=false

while [[ $# -gt 0 ]]; do
  case $1 in
    --full) FULL=true; shift ;;
    --threshold) THRESHOLD="$2"; shift 2 ;;
    *) echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

# Get API token from config
CONFIG="/Users/seppa/.hindsight/coding-agent.json"
API_URL="https://hindsight.labb.site"
TOKEN=$(python3 -c "import json; print(json.load(open('$CONFIG'))['apiToken'])" 2>/dev/null)

if [[ -z "$TOKEN" || "$TOKEN" == "null" ]]; then
  echo "Error: API token not found in $CONFIG" >&2
  exit 1
fi

echo "Fetching all Hindsight pages..."

# Fetch pages list
PAGES_JSON=$(curl -sSf -H "Authorization: Bearer $TOKEN" "$API_URL/api/v1/pages" 2>/dev/null)

if [[ -z "$PAGES_JSON" || "$PAGES_JSON" == "[]" ]]; then
  echo "No pages found."
  exit 0
fi

# Parse into arrays
mapfile -t PAGE_IDS < <(echo "$PAGES_JSON" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for p in data:
    print(p['id'])
")
mapfile -t PAGE_TITLES < <(echo "$PAGES_JSON" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for p in data:
    print(p['title'])
")
mapfile -t PAGE_DESCS < <(echo "$PAGES_JSON" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for p in data:
    print(p.get('description', '')[:500])
")

N=${#PAGE_IDS[@]}
echo "Found $N pages. Comparing..."

# For full mode, fetch full content of each page
if [[ "$FULL" == "true" ]]; then
  echo "Fetching full content (this may take a while)..."
  declare -a PAGE_CONTENTS
  for id in "${PAGE_IDS[@]}"; do
    content=$(curl -sSf -H "Authorization: Bearer $TOKEN" "$API_URL/api/v1/pages/$id" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('content','')[:5000])" 2>/dev/null || echo "")
    PAGE_CONTENTS+=("$content")
  done
fi

# Compare pairwise
found=false
for ((i=0; i<N; i++)); do
  for ((j=i+1; j<N; j++)); do
    title1="${PAGE_TITLES[i]}"
    title2="${PAGE_TITLES[j]}"
    desc1="${PAGE_DESCS[i]}"
    desc2="${PAGE_DESCS[j]}"
    
    # Quick title similarity (case-insensitive, word overlap)
    t1_words=$(echo "$title1" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' ' ' | tr ' ' '\n' | sort -u)
    t2_words=$(echo "$title2" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' ' ' | tr ' ' '\n' | sort -u)
    
    common_title=$(comm -12 <(echo "$t1_words") <(echo "$t2_words") | wc -l)
    total_title=$(echo -e "$t1_words\n$t2_words" | sort -u | wc -l)
    [[ $total_title -eq 0 ]] && title_sim=0 || title_sim=$((common_title * 100 / total_title))
    
    # Description similarity
    d1_words=$(echo "$desc1" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' ' ' | tr ' ' '\n' | sort -u)
    d2_words=$(echo "$desc2" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' ' ' | tr ' ' '\n' | sort -u)
    common_desc=$(comm -12 <(echo "$d1_words") <(echo "$d2_words") | wc -l)
    total_desc=$(echo -e "$d1_words\n$d2_words" | sort -u | wc -l)
    [[ $total_desc -eq 0 ]] && desc_sim=0 || desc_sim=$((common_desc * 100 / total_desc))
    
    # Combined score (weight title more heavily)
    combined=$(( (title_sim * 3 + desc_sim * 2) / 5 ))
    
    if [[ $combined -ge $THRESHOLD ]]; then
      found=true
      echo ""
      echo "=== OVERLAP DETECTED (${combined}%) ==="
      echo "  Page 1: ${PAGE_IDS[i]} — \"$title1\""
      echo "  Page 2: ${PAGE_IDS[j]} — \"$title2\""
      echo "  Title sim: ${title_sim}% | Desc sim: ${desc_sim}%"
      echo "  Suggested: hindsight_capture_initiative(title=\"$title1\", summary=\"<merged>\", relates_to_page_id=\"${PAGE_IDS[i]}\")"
      
      if [[ "$FULL" == "true" ]]; then
        # Full content comparison
        c1="${PAGE_CONTENTS[i]}"
        c2="${PAGE_CONTENTS[j]}"
        c1_words=$(echo "$c1" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' ' ' | tr ' ' '\n' | sort -u)
        c2_words=$(echo "$c2" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' ' ' | tr ' ' '\n' | sort -u)
        common_full=$(comm -12 <(echo "$c1_words") <(echo "$c2_words") | wc -l)
        total_full=$(echo -e "$c1_words\n$c2_words" | sort -u | wc -l)
        [[ $total_full -eq 0 ]] && full_sim=0 || full_sim=$((common_full * 100 / total_full))
        echo "  Full content sim: ${full_sim}%"
      fi
    fi
  done
done

if [[ "$found" == "false" ]]; then
  echo "No overlaps >= ${THRESHOLD}% found."
fi

echo ""
echo "Done. Re-run with --full for deeper analysis, or --threshold N to adjust sensitivity."