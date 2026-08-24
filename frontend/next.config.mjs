/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // The API lives in a separate service, so nothing here needs image
  // optimisation or a server runtime beyond rendering the shell.
  poweredByHeader: false,
};

export default nextConfig;
