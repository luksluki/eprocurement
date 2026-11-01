import eslint from "@eslint/js";
import tseslint from "typescript-eslint";

// Minimal flat config for the Playwright suite: recommended JS rules plus
// type-aware-free TypeScript rules. The app packages keep their own Next
// configs; the e2e package only ships specs and support code.
export default tseslint.config(
  { ignores: ["node_modules/**", "playwright-report/**", "test-results/**"] },
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  {
    rules: {
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_" }],
    },
  },
);
