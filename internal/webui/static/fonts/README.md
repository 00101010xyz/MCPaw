# Bundled typefaces

MCPaw serves its own fonts. The admin interface runs under a strict
Content-Security-Policy whose `font-src` is `'self'`, so a CDN link would simply
be blocked — and an infrastructure console should not phone home to a third
party on every page load anyway.

These are latin-subset **variable** woff2 files pulled from the upstream
projects. One file per family covers every weight the stylesheet asks for.

| File | Family | Weights used | Licence |
| --- | --- | --- | --- |
| `spacegrotesk-var.woff2` | Space Grotesk | 500, 600 | SIL Open Font License 1.1 |
| `inter-var.woff2` | Inter | 400, 500, 600 | SIL Open Font License 1.1 |
| `jetbrainsmono-var.woff2` | JetBrains Mono | 400, 500 | SIL Open Font License 1.1 |

All three are licensed under the SIL Open Font License, Version 1.1, which
permits redistribution as part of this software. The full licence text is at
<https://openfontlicense.org>. The fonts are unmodified apart from subsetting to
the latin range.
