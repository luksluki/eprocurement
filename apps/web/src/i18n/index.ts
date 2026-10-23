// UI copy for the ID/EN locales (PRD §19: cookie-based ID + EN). Client
// components never import this module's dictionaries — server components read
// them and pass strings down as props, so packages/ui stays presentational
// and the client bundle stays dictionary-agnostic.

export const LOCALES = ["id", "en"] as const;
export type Locale = (typeof LOCALES)[number];

// eproc_* cookie naming follows the session cookies (lib/constants.ts); the
// value is not sensitive, so it is a plain readable cookie.
export const LOCALE_COOKIE = "eproc_locale";

// Default per the PRD's "ID + EN" ordering; flipping to "en" is a one-line
// change if review prefers it.
export const DEFAULT_LOCALE: Locale = "id";

export function isLocale(value: unknown): value is Locale {
  return value === "id" || value === "en";
}

export type PageCopy = {
  title: string;
  description: string;
  milestone: string;
};

export type Dictionary = {
  brand: { name: string; initials: string };
  nav: {
    ariaLabel: string;
    dashboard: string;
    requisitions: string;
    approvals: string;
    purchaseOrders: string;
    vendors: string;
    invoices: string;
    reports: string;
    admin: string;
    users: string;
    settings: string;
  };
  header: {
    tenantLabel: string;
    noRoles: string;
    signOut: string;
    signingOut: string;
    switchTenant: string;
    switchPlaceholder: string;
    switchingTenant: string;
    switchFailed: string;
    switchNetworkError: string;
    language: string;
  };
  login: {
    heading: string;
    subtitle: string;
    email: string;
    password: string;
    submit: string;
    submitting: string;
    invalid: string;
    network: string;
    switchFailed: string;
  };
  placeholder: {
    notBuilt: string;
    plannedIn: (milestone: string) => string;
  };
  adminGate: { message: string };
  dashboard: {
    title: string;
    kpi: ReadonlyArray<{ label: string; note: string }>;
    chartNote: string;
  };
  vendors: {
    title: string;
    description: string;
    caption: string;
    unavailable: string;
    empty: string;
    emptyHint: string;
    columns: { code: string; name: string; contact: string; status: string };
    pagination: {
      nav: string;
      previous: string;
      next: string;
      summary: (range: { from: number; to: number; total: number; page: number; pageCount: number }) => string;
    };
  };
  pages: {
    requisitions: PageCopy;
    approvals: PageCopy;
    purchaseOrders: PageCopy;
    invoices: PageCopy;
    reports: PageCopy;
    users: PageCopy;
    settings: PageCopy;
  };
  localeNames: { id: string; en: string };
};

const id: Dictionary = {
  brand: { name: "eProcurement", initials: "eP" },
  nav: {
    ariaLabel: "Navigasi utama",
    dashboard: "Dasbor",
    requisitions: "Permintaan Pembelian",
    approvals: "Persetujuan",
    purchaseOrders: "Pesanan Pembelian",
    vendors: "Pemasok",
    invoices: "Faktur",
    reports: "Laporan",
    admin: "Admin",
    users: "Pengguna",
    settings: "Pengaturan",
  },
  header: {
    tenantLabel: "Tenant",
    noRoles: "tanpa peran",
    signOut: "Keluar",
    signingOut: "Keluar…",
    switchTenant: "Tenant",
    switchPlaceholder: "Ganti…",
    switchingTenant: "Mengganti…",
    switchFailed: "Penggantian tenant gagal.",
    switchNetworkError: "Kesalahan jaringan saat mengganti tenant.",
    language: "Bahasa",
  },
  login: {
    heading: "Masuk",
    subtitle: "Masuk ke ruang kerja Anda",
    email: "Email",
    password: "Kata sandi",
    submit: "Masuk",
    submitting: "Sedang masuk…",
    invalid: "Gagal masuk. Coba lagi.",
    network: "Kesalahan jaringan — apakah gateway berjalan? (GATEWAY_URL)",
    switchFailed: "Penggantian tenant gagal. Masuk secara manual.",
  },
  placeholder: {
    notBuilt: "Layar ini belum dibangun.",
    plannedIn: (milestone) => `Direncanakan pada ${milestone}.`,
  },
  adminGate: { message: "Hanya administrator." },
  dashboard: {
    title: "Dasbor",
    kpi: [
      { label: "Permintaan terbuka", note: "tiba bersama UI permintaan" },
      { label: "Persetujuan tertunda", note: "tiba bersama antrean persetujuan" },
      { label: "Pesanan pembelian aktif", note: "tiba bersama layar PO" },
      { label: "Belanja bulan ini", note: "tiba bersama laporan" },
    ],
    chartNote: "Grafik belanja dan daftar persetujuan tertunda tiba bersama tonggak analitik.",
  },
  vendors: {
    title: "Pemasok",
    description: "Data induk pemasok terlingkup tenant. Pemasok lintas tenant tidak pernah terlihat.",
    caption: "Daftar pemasok",
    unavailable: "Gagal memuat pemasok — apakah gateway berjalan dan token akses valid?",
    empty: "Belum ada pemasok.",
    emptyHint:
      "Lihat “Admin tenant demo” di docs/onboarding.md untuk menyemai data pemasok per tenant.",
    columns: { code: "Kode", name: "Nama", contact: "Kontak", status: "Status" },
    pagination: {
      nav: "Paginasi",
      previous: "Sebelumnya",
      next: "Berikutnya",
      summary: ({ from, to, total }) =>
        `Menampilkan ${from.toLocaleString("id-ID")}–${to.toLocaleString("id-ID")} dari ${total.toLocaleString("id-ID")} pemasok`,
    },
  },
  pages: {
    requisitions: {
      title: "Permintaan Pembelian",
      description:
        "Daftar permintaan pembelian dengan filter dan paginasi, plus formulir pembuatan multi-langkah (rincian → item → lampiran → tinjau).",
      milestone: "M2 — CRUD PR & mesin persetujuan",
    },
    approvals: {
      title: "Persetujuan",
      description:
        "Antrean pemberi persetujuan: langkah tertunda dengan ambang batas, setujui/tolak dengan alasan.",
      milestone: "M2 — mesin persetujuan",
    },
    purchaseOrders: {
      title: "Pesanan Pembelian",
      description:
        "Daftar dan detail PO dengan aksi terbit/konfirmasi serta penerimaan barang (penerimaan parsial didukung, kelebihan ditolak).",
      milestone: "M3 — PO & GRN",
    },
    invoices: {
      title: "Faktur",
      description: "Daftar dan detail faktur dengan hasil pencocokan 3-arah dan konfigurasi toleransi.",
      milestone: "M3 — pencocokan faktur",
    },
    reports: {
      title: "Laporan",
      description: "Ekspor, pekerjaan terjadwal, dan riwayat eksekusi (ekspor CSV malam hari per tenant ke MinIO).",
      milestone: "M4 — ekspor & pekerjaan",
    },
    users: {
      title: "Pengguna",
      description: "Administrasi pengguna dan peran untuk tenant ini.",
      milestone: "tonggak admin identitas",
    },
    settings: {
      title: "Pengaturan",
      description: "Konfigurasi aturan persetujuan dan toleransi.",
      milestone: "tonggak konfigurasi persetujuan",
    },
  },
  localeNames: { id: "Bahasa Indonesia", en: "English" },
};

const en: Dictionary = {
  brand: { name: "eProcurement", initials: "eP" },
  nav: {
    ariaLabel: "Main navigation",
    dashboard: "Dashboard",
    requisitions: "Requisitions",
    approvals: "Approvals",
    purchaseOrders: "Purchase Orders",
    vendors: "Vendors",
    invoices: "Invoices",
    reports: "Reports",
    admin: "Admin",
    users: "Users",
    settings: "Settings",
  },
  header: {
    tenantLabel: "Tenant",
    noRoles: "no roles",
    signOut: "Sign out",
    signingOut: "Signing out…",
    switchTenant: "Tenant",
    switchPlaceholder: "Switch…",
    switchingTenant: "Switching…",
    switchFailed: "Switch failed.",
    switchNetworkError: "Network error during tenant switch.",
    language: "Language",
  },
  login: {
    heading: "Sign in",
    subtitle: "Sign in to your workspace",
    email: "Email",
    password: "Password",
    submit: "Sign in",
    submitting: "Signing in…",
    invalid: "Sign-in failed. Try again.",
    network: "Network error — is the gateway running? (GATEWAY_URL)",
    switchFailed: "Tenant switch failed. Sign in manually.",
  },
  placeholder: {
    notBuilt: "This screen is not built yet.",
    plannedIn: (milestone) => `Planned in ${milestone}.`,
  },
  adminGate: { message: "Administrators only." },
  dashboard: {
    title: "Dashboard",
    kpi: [
      { label: "Open requisitions", note: "arrives with the requisition UI" },
      { label: "Pending approvals", note: "arrives with the approval queue" },
      { label: "Active purchase orders", note: "arrives with the PO screens" },
      { label: "Spend this month", note: "arrives with reports" },
    ],
    chartNote: "Spend chart and pending-approval list land with the analytics milestone.",
  },
  vendors: {
    title: "Vendors",
    description: "Tenant-scoped vendor master. Cross-tenant vendors are never visible.",
    caption: "Vendor list",
    unavailable: "Failed to load vendors — is the gateway running and the access token valid?",
    empty: "No vendors yet.",
    emptyHint: "See “Demo tenant admins” in docs/onboarding.md for seeding vendor data per tenant.",
    columns: { code: "Code", name: "Name", contact: "Contact", status: "Status" },
    pagination: {
      nav: "Pagination",
      previous: "Previous",
      next: "Next",
      summary: ({ from, to, total }) =>
        `Showing ${from.toLocaleString("en-US")}–${to.toLocaleString("en-US")} of ${total.toLocaleString("en-US")} vendors`,
    },
  },
  pages: {
    requisitions: {
      title: "Requisitions",
      description:
        "Purchase requisition list with filters and pagination, plus the multi-step create form (details → items → attachments → review).",
      milestone: "M2 — PR CRUD & approval engine",
    },
    approvals: {
      title: "Approvals",
      description: "The approver's queue: pending steps with thresholds, approve/reject with reason.",
      milestone: "M2 — approval engine",
    },
    purchaseOrders: {
      title: "Purchase Orders",
      description:
        "PO list and detail with issue/acknowledge actions and goods receipts (partial receipts supported, over-receipt rejected).",
      milestone: "M3 — PO & GRN",
    },
    invoices: {
      title: "Invoices",
      description: "Invoice list and detail with 3-way match results and tolerance configuration.",
      milestone: "M3 — invoice matching",
    },
    reports: {
      title: "Reports",
      description: "Exports, scheduled jobs, and run history (nightly CSV export per tenant to MinIO).",
      milestone: "M4 — exports & jobs",
    },
    users: {
      title: "Users",
      description: "Users and roles administration for this tenant.",
      milestone: "the identity admin milestone",
    },
    settings: {
      title: "Settings",
      description: "Approval rules and tolerances configuration.",
      milestone: "the approval configuration milestone",
    },
  },
  localeNames: { id: "Bahasa Indonesia", en: "English" },
};

export const dictionaries: Record<Locale, Dictionary> = { id, en };

export function getDictionary(locale: Locale): Dictionary {
  return dictionaries[locale];
}
