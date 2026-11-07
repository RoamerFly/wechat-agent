# Agent home icon

`agent-home.png` is the transparent GPT Image source generated on 2026-11-07.
`agent-home.ico` contains 16, 20, 24, 32, 40, 48, 64, 96, 128 and 256 pixel frames.
Windows uses the ICO for the executable, window and tray. The startup and shutdown
surface embeds the same PNG. `scripts/build_windows_icon.py` converts the source
into ICO frames and verifies alpha and sizes. Conversion does not call an image API.

The chosen theme is a warm house with two abstract companions, an indigo roof
and amber doorway. Generation used the built-in GPT Image tool.

## Generation prompt

Create a production Windows desktop app icon asset, square canvas, transparent
outside the icon. App is a warm home station for a family of personal AI agents.
Design a highly recognizable compact midnight indigo rounded-square tile, with
a single friendly small home silhouette inside, a warm golden lit open doorway
and two round abstract AI companions, pale lavender and warm peach, one slightly
taller. Simple crisp optical hierarchy, distinctive silhouette at 16 px, broad
shapes, navy, soft periwinkle and amber, generous transparent outer 8% margin.
Slight handcrafted bevel and shading, cozy at night, clean rounded contours.
No lettering, badges, noise, thin linework, mockups, human or animal faces.
Centered artwork with no external shadow and minimal internal highlights.
