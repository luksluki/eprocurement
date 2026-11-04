import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  reactStrictMode: true,
  // The design system ships TypeScript sources (main: src/index.ts), so Next
  // must transpile it like the app itself.
  transpilePackages: ["@eprocurement/ui"],
  // CI runs ESLint via `pnpm lint` (eslint .); keep `next build` focused on
  // compiling so the two checks stay independent and fast.
  eslint: {
    ignoreDuringBuilds: true,
  },
};

export default nextConfig;
