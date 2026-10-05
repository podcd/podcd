# Website

[Docusaurus](https://docusaurus.io/) site.

```console
npm install
npm start          # dev server with live reload
npm run build      # static site into build/; CI builds on PRs touching docs/, deploys main to GitHub Pages
```

## Versions

`docs/` tracks `main`. To freeze the docs for a release, once they are final:

```console
npm run docusaurus docs:version 2.3
```

This copies `docs/` into `versioned_docs/version-2.3/` and adds `2.3` to the version dropdown.
