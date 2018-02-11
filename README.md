build
=====

Simple build tool to replace make.

Features:

- Artifact recipes are isolated and atomic.  Each recipe is run in an isolated
  build directory, where it can only see dependencies it's explicitly included,
  and can't modify any files outside the directory.  Instead each recipe
  produces an artifact.  If the recipe fails, the artifact isn't created at all
  rather than a partial artifact.
- Automated watching and serving.  `build watch target` will build target, then
  automatically rebuild it if any dependencies change.  There can also be serve
  targets.  For instance `build watch serve-api` might serve an API over HTTP
  locally; if any dependencies change the server will automatically be restart
  with the new changes.
- Automatically parallel.  Since recipes are isolated, they will be safely and
  automatically run in parallel where possible.
- YAML, so you don't have to learn a new syntax.

The build configuration for a project is specified in a `build.yml` file.
Here's an example of one:

	node_modules:
		- include node_modules package.json
		- yarn
		- mv node_modules $OUT

	index.css:
		- include node_modules index.scss
		- node_modules/.bin/sass index.scss >$OUT
	
	bundle.js:
		- include node_modules app.ts
		- node_modules/.bin/tslint app.ts
		- node_modules/.bin/tsc --strictNullChecks --noImplicitAny app.ts
		- browserify app.ts >$OUT

	dist:
		- mkdir $OUT
		- include index.html index.css bundle.js -into $OUT

	serve-dist:
		- include dist
		- serve-http dist

A recipe runs in an isolated directory.  When the special `include` command is
used, its arguments will be built if necessary and then linked into the
isolated directory.  The build step places its output into the special $OUT
path, which can refer to either a file or directory.

Build steps prefixed with `serve-` are special.  They are expected to not
produce any outputs, but instead start a long-running process like an HTTP or
API server.  If any of the dependencies change, the server will automatically
be restarted with the new changes.
