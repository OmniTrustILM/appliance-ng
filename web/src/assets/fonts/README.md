# Inter

`inter-latin.woff2` is the Latin subset of Inter Variable 4.001 (`Inter-VariableFont_opsz,wght.woff2`), taken
from the CZERTAINLY administrator front end. Inter is licensed under the SIL Open Font License 1.1; see
[OFL.txt](OFL.txt).

Subset with fonttools and brotli:

```
pyftsubset Inter-VariableFont_opsz,wght.woff2 --output-file=inter-latin.woff2 --flavor=woff2 --layout-features='*' \
  --unicodes='U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+2000-206F,U+2074,U+20AC,U+2122,U+2190-2193,U+2212,U+2215,U+2713,U+FEFF,U+FFFD'
```
