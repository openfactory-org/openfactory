# Brand assets

The Open Factory logo is two equal squares that touch at one corner: a dark square at the bottom left and an orange square at the top right.

## Colors

| Name | Hex | Use |
|---|---|---|
| Ink | `#14171F` | Dark square, text, dark backgrounds |
| Orange | `#FF5A1F` | Accent square |
| Paper | `#F4F1EA` | Light backgrounds, the square on dark backgrounds |

## Files

| File | Use |
|---|---|
| `logo-mark.svg` | The mark on light backgrounds. Transparent. |
| `logo-mark-dark.svg` | The mark on dark backgrounds. Transparent. |
| `icon.svg` | App icon: the mark on a rounded paper tile. Also the dashboard favicon. |
| `icon-dark.svg` | App icon on an ink tile. |
| `icon-square.svg` | Icon with square corners, for platforms that round the corners themselves. |
| `favicon.ico` | 16, 32 and 48 px favicon for sites that need `.ico`. |
| `png/icon-512.png`, `png/icon-192.png`, `png/icon-dark-512.png` | Raster app icons. |
| `png/favicon-32.png`, `png/favicon-16.png` | Raster favicons. |
| `png/apple-touch-icon.png` | 180 px iOS home screen icon. |
| `png/logo-mark-512.png`, `png/logo-mark-dark-512.png` | Raster marks. Transparent. |
| `png/lockup-light.png`, `png/lockup-dark.png` | Mark with the wordmark, 1200 x 300. Transparent. |
| `png/social-card.png` | 1200 x 630 link preview image. GitHub uses it as the social preview: set it in the repository settings. |

The dashboard uses copies in [`web/public/`](../../../web/public/): `favicon.svg` (same as `icon.svg`) and `apple-touch-icon.png`. The repository README shows `logo-mark.svg` and `logo-mark-dark.svg`.

## Wordmark

The wordmark is "openfactory" in Space Grotesk SemiBold, lowercase, with -0.03em letter spacing. The lockups are PNG only, because an SVG with live text needs the font installed.

## Rules

- Keep the two squares the same size and touching at the corner.
- Keep clear space of at least half a square on every side.
- Use only the three colors above.
