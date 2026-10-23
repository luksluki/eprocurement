import { redirect } from "next/navigation";

// PRD §11: "/" redirects to the dashboard.
export default function Home() {
  redirect("/app/dashboard");
}
