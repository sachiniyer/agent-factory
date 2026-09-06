#!/usr/bin/env bash
# Losslessly re-encode recorder PNGs; keep the original when it is smaller.
set -euo pipefail
[ -f /.dockerenv ] || [ -f /run/.containerenv ] || { echo 'container only' >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for image in "$1"/*.png; do
    candidate="$work/optimized.png"
    ffmpeg -nostdin -y -loglevel error -i "$image" \
        -compression_level 9 -pred mixed "$candidate"
    if [ "$(wc -c < "$candidate")" -lt "$(wc -c < "$image")" ]; then
        mv "$candidate" "$image"
    else
        rm "$candidate"
    fi
done

# Preserve the twelve-still aggregate published in #3884.
tour_bytes=0
for scene in dashboard new-session agent-tab review tasks config-accounts; do
    for mode in '' -dark; do
        tour_bytes=$((tour_bytes + $(wc -c < "$1/$scene$mode.png")))
    done
done
if [ "$tour_bytes" -gt 906092 ]; then
    echo "Tour stills exceed the 906092-byte budget: $tour_bytes" >&2
    exit 1
fi
printf 'Tour stills: %s / 906092 bytes\n' "$tour_bytes"
