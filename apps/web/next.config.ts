import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  reactStrictMode: true,
  // CI runs ESLint via `pnpm lint` (eslint .); keep `next build` focused on
  // compiling so the two checks stay independent and fast.
  eslint: {
    ignoreDuringBuilds: true,
  },
};

export default nextConfig;
