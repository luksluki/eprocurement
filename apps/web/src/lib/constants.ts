// Session cookie names. Both are httpOnly and written only by route handlers
// (Next.js allows cookies().set() there exclusively — server components never
// write cookies). Import-safe for the middleware edge bundle: no server-only
// modules here.
export const ACCESS_COOKIE = "eproc_at";
export const REFRESH_COOKIE = "eproc_rt";
