# Print Agent dashboard

React 19 + TypeScript admin dashboard for the local print agent. Vite serves
the application during development and writes the production build to
`../dist`, which Go embeds into the executable.

## Commands

```sh
npm ci
npm run dev
npm run typecheck
npm run lint
npm test
npm run build
npm run check
```

The development server uses `/admin/` as its base path and proxies `/api/v1`
to `http://127.0.0.1:17432`. Start the Go agent separately when exercising
real printers. Submitting or reprinting a Job can produce physical output.

Production assets are content-hashed and generated under the ignored
`../dist` directory. Run `npm run build` before compiling the Go application;
commit the dashboard source, not the generated assets.
