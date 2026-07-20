# Architecture diagram sources

The diagrams are deterministic, repository-owned SVG compositions. Generic
component symbols are selected from Tabler Icons 3.45.0 and inlined into the
SVG output; see `ICON-LICENSE.txt`.

Regenerate the SVG files and high-resolution PNG fallbacks from this directory:

```sh
node render-architecture.mjs
rsvg-convert --zoom=2 system-architecture.svg --output system-architecture.png
rsvg-convert --zoom=2 data-and-persistence.svg --output data-and-persistence.png
```

The Canary logo is read from `../social/canary-icon.png` during generation.
