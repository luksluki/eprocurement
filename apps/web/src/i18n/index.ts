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
    audit: string;
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
    description: string;
    unavailable: string;
    kpi: {
      pending: string;
      spend30: string;
      spend90: string;
      poTotal: string;
      pendingNote: string;
      spend30Note: string;
      spend90Note: string;
      poTotalNote: string;
    };
    approvals: {
      heading: string;
      caption: string;
      empty: string;
      hidden: (hidden: number) => string;
      columns: { number: string; submitted: string; total: string; step: string; role: string };
    };
    spend: {
      heading: string;
      caption: string;
      empty: string;
      columns: { vendor: string; thirty: string; ninety: string };
    };
    distribution: {
      heading: string;
      caption: string;
      total: string;
    };
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
  requisitionForm: {
    title: string;
    description: string;
    newLink: string;
    steps: { details: string; items: string; attachments: string; review: string };
    details: {
      heading: string;
      currencyLabel: string;
      currencyPlaceholder: string;
      currencyLockedNote: string;
      next: string;
    };
    items: {
      heading: string;
      description: string;
      qty: string;
      unitPrice: string;
      category: string;
      add: string;
      remove: string;
      next: string;
      back: string;
    };
    attachments: {
      heading: string;
      hint: string;
      add: string;
      empty: string;
      uploading: string;
      next: string;
      back: string;
    };
    review: {
      heading: string;
      itemsHeading: string;
      attachmentsHeading: string;
      total: string;
      submit: string;
      submitting: string;
      back: string;
      itemsEmpty: string;
    };
    autosave: { saving: string; saved: string; failed: string };
    restore: { loading: string; notFound: string; error: string; notDraft: string };
    submitted: { heading: string; backToList: string };
  };
  timeline: {
    title: string;
    empty: string;
    loading: string;
    sessionExpired: string;
    error: string;
    retry: string;
    loadMore: string;
    loadingMore: string;
    filterLabel: string;
    filterAll: string;
  };
  requisitionDetail: {
    backToList: string;
    print: string;
    notFound: string;
    loadFailed: string;
    fields: {
      requester: string;
      currency: string;
      total: string;
      submitted: string;
      created: string;
    };
    items: {
      heading: string;
      description: string;
      category: string;
      qty: string;
      unitPrice: string;
      lineTotal: string;
      empty: string;
    };
    steps: {
      heading: string;
      step: string;
      roleRequired: string;
      threshold: string;
      status: string;
      decidedBy: string;
      decidedAt: string;
      reason: string;
      empty: string;
    };
    attachments: {
      heading: string;
      empty: string;
    };
    actions: {
      approve: string;
      reject: string;
      reasonLabel: string;
      reasonPlaceholder: string;
      reasonRequired: string;
      reasonTooLong: string;
      submitting: string;
      genericError: string;
      sessionExpired: string;
      errorMessages: Record<string, string>;
    };
  };
  purchaseOrders: {
    title: string;
    description: string;
    caption: string;
    unavailable: string;
    empty: string;
    emptyHint: string;
    columns: { number: string; status: string; vendor: string; currency: string; total: string; issued: string; created: string };
    filters: { numberLabel: string; numberPlaceholder: string; statusLabel: string; statusAll: string; pageSizeLabel: string; apply: string };
    pagination: {
      nav: string;
      previous: string;
      next: string;
      summary: (range: { from: number; to: number; total: number; page: number; pageCount: number }) => string;
    };
  };
  purchaseOrderDetail: {
    backToList: string;
    notFound: string;
    loadFailed: string;
    fields: {
      vendor: string;
      currency: string;
      total: string;
      issued: string;
      created: string;
      requisition: string;
    };
    items: {
      heading: string;
      description: string;
      qtyOrdered: string;
      qtyReceived: string;
      remaining: string;
      unitPrice: string;
      lineTotal: string;
    };
    actions: {
      issue: string;
      acknowledge: string;
      submitting: string;
      genericError: string;
      sessionExpired: string;
      errorMessages: Record<string, string>;
      // The invoice flow's entry point (#41, maintainer decision
      // 2026-10-29): finance on an invoicable PO.
      createInvoice: string;
    };
    receipt: {
      heading: string;
      hint: string;
      qtyLabel: string;
      notesLabel: string;
      notesPlaceholder: string;
      submit: string;
      submitting: string;
      qtyInvalid: string;
      // Template strings with {placeholders} — the client component
      // interpolates (functions cannot cross the server→client boundary).
      qtyOver: string;
      minOneLine: string;
      notesTooLong: string;
      successTitle: string;
      sessionExpired: string;
      genericError: string;
      errorMessages: Record<string, string>;
      overReceipt: string;
    };
  };
  invoices: {
    title: string;
    description: string;
    caption: string;
    unavailable: string;
    empty: string;
    emptyHint: string;
    columns: { number: string; status: string; vendor: string; currency: string; total: string; created: string };
    filters: { numberLabel: string; numberPlaceholder: string; statusLabel: string; statusAll: string; pageSizeLabel: string; apply: string };
    pagination: {
      nav: string;
      previous: string;
      next: string;
      summary: (range: { from: number; to: number; total: number; page: number; pageCount: number }) => string;
    };
  };
  invoiceDetail: {
    backToList: string;
    notFound: string;
    loadFailed: string;
    fields: {
      vendor: string;
      currency: string;
      total: string;
      po: string;
      poLink: string;
      created: string;
    };
    items: {
      heading: string;
      poItem: string;
      qty: string;
      unitPrice: string;
      lineTotal: string;
    };
    actions: {
      match: string;
      submitting: string;
      gated: string;
      genericError: string;
      sessionExpired: string;
      errorMessages: Record<string, string>;
    };
    attachments: {
      heading: string;
      empty: string;
      columns: { filename: string; size: string; status: string; download: string };
      downloadFailed: string;
      uploadLabel: string;
      uploading: string;
    };
    match: {
      heading: string;
      verdictMatched: string;
      verdictDiscrepancy: string;
      pending: string;
      invalid: string;
      columns: {
        poItem: string;
        qtyOrdered: string;
        qtyReceived: string;
        qtyInvoiced: string;
        qtyDelta: string;
        poPrice: string;
        invoicePrice: string;
        priceDelta: string;
        flags: string;
        reason: string;
      };
      qtyBreach: string;
      priceBreach: string;
      ok: string;
      // Template strings with {placeholders} — interpolated with the numbers
      // parsed from the pinned reason lines.
      reasonQty: string;
      reasonPrice: string;
    };
  };
  invoiceForm: {
    title: string;
    back: string;
    missingPO: string;
    poNotFound: string;
    loadFailed: string;
    notAllowed: string;
    form: {
      heading: string;
      numberLabel: string;
      numberPlaceholder: string;
      itemsHeading: string;
      itemsHint: string;
      itemsLine: string;
      itemsQty: string;
      itemsUnitPrice: string;
      itemsLineTotal: string;
      submit: string;
      submitting: string;
      numberInvalid: string;
      qtyInvalid: string;
      priceInvalid: string;
      minOneLine: string;
      genericError: string;
      sessionExpired: string;
      errorMessages: Record<string, string>;
      // Template with {poNumber}/{invoiced}/{requested}/{total} — the parsed
      // over-bill detail line, amounts pre-formatted as money.
      overBill: string;
    };
  };
  pages: {
    requisitions: PageCopy;
    approvals: PageCopy;
    purchaseOrders: PageCopy;
    invoices: PageCopy;
    reports: PageCopy;
    audit: PageCopy;
    users: PageCopy;
    settings: PageCopy;
  };
  // Audit page copy (#44): the decision log's table, filters, keyset
  // pagination, and CSV export — all rendered by the server component.
  auditPage: {
    caption: string;
    unavailable: string;
    forbidden: string;
    empty: string;
    emptyHint: string;
    columns: {
      occurredAt: string;
      actor: string;
      action: string;
      entity: string;
      ip: string;
      trace: string;
    };
    filters: { actionLabel: string; actionAll: string; apply: string };
    pagination: { next: string; first: string };
    exportCsv: string;
    exportHint: string;
  };
  // ExportRunsTable copy (#49): passed as props by the reports page —
  // client components never import the dictionaries (the Timeline pattern).
  reportsTable: {
    loading: string;
    error: string;
    retry: string;
    sessionExpired: string;
    empty: string;
    download: string;
    downloading: string;
    downloadFailed: string;
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
    audit: "Log Audit",
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
    description:
      "Kesehatan operasional tenant: antrean persetujuan, belanja per pemasok, dan distribusi status pesanan pembelian.",
    unavailable: "Gagal memuat ringkasan dasbor — apakah gateway berjalan dan token akses valid?",
    kpi: {
      pending: "Persetujuan tertunda",
      spend30: "Belanja 30 hari",
      spend90: "Belanja 90 hari",
      poTotal: "Total pesanan pembelian",
      pendingNote: "langkah persetujuan berjalan saat ini",
      spend30Note: "PO terbit 30 hari terakhir",
      spend90Note: "PO terbit 90 hari terakhir",
      poTotalNote: "seluruh status FR-06",
    },
    approvals: {
      heading: "Persetujuan tertunda",
      caption: "Antrean persetujuan tertunda, terlama dikirim lebih dulu",
      empty: "Tidak ada persetujuan tertunda.",
      hidden: (hidden) => `+ ${hidden.toLocaleString("id-ID")} lainnya di antrean`,
      columns: {
        number: "Nomor",
        submitted: "Dikirim",
        total: "Total",
        step: "Langkah",
        role: "Peran penyetuju",
      },
    },
    spend: {
      heading: "Belanja per pemasok",
      caption: "Total PO terbit per pemasok dan mata uang",
      empty: "Belum ada belanja PO terbit dalam 90 hari terakhir.",
      columns: { vendor: "Pemasok", thirty: "30 hari", ninety: "90 hari" },
    },
    distribution: {
      heading: "Distribusi status PO",
      caption: "Hitungan pesanan pembelian per status FR-06",
      total: "Total",
    },
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
  requisitionForm: {
    title: "Permintaan Pembelian baru",
    description:
      "Empat langkah: rincian → item → lampiran → tinjau. Draf tersimpan otomatis ke server dan dipulihkan saat halaman dimuat ulang.",
    newLink: "Buat permintaan baru",
    steps: { details: "Rincian", items: "Item", attachments: "Lampiran", review: "Tinjau" },
    details: {
      heading: "Rincian permintaan",
      currencyLabel: "Mata uang",
      currencyPlaceholder: "Pilih mata uang…",
      currencyLockedNote:
        "Mata uang terkunci setelah draf dibuat (lampiran terikat pada draf ini). Untuk mengubahnya, mulai draf baru.",
      next: "Lanjut ke item",
    },
    items: {
      heading: "Item permintaan",
      description: "Deskripsi",
      qty: "Jumlah",
      unitPrice: "Harga satuan (sen)",
      category: "Kategori",
      add: "Tambah item",
      remove: "Hapus item",
      next: "Lanjut ke lampiran",
      back: "Kembali",
    },
    attachments: {
      heading: "Lampiran",
      hint: "Berkas diunggah langsung ke penyimpanan lewat URL presigned (pdf, png, jpeg, webp, docx, xlsx; maks 25 MB).",
      add: "Pilih berkas",
      empty: "Belum ada lampiran.",
      uploading: "Mengunggah…",
      next: "Lanjut ke tinjau",
      back: "Kembali",
    },
    review: {
      heading: "Tinjau dan kirim",
      itemsHeading: "Item",
      attachmentsHeading: "Lampiran",
      total: "Total (sen)",
      submit: "Kirim untuk disetujui",
      submitting: "Mengirim…",
      back: "Kembali",
      itemsEmpty: "Tambahkan minimal satu item sebelum mengirim.",
    },
    autosave: { saving: "Menyimpan draf…", saved: "Draf tersimpan.", failed: "Gagal menyimpan draf." },
    restore: {
      loading: "Memulihkan draf…",
      notFound: "Draf tidak ditemukan (atau milik tenant lain).",
      error: "Gagal memuat draf.",
      notDraft:
        "Permintaan ini sudah bukan draf (mungkin sudah dikirim dari tab lain) — formulir bersifat baca-saja dan autosave dihentikan.",
    },
    submitted: { heading: "Permintaan terkirim:", backToList: "Kembali ke daftar permintaan" },
  },
  timeline: {
    title: "Linimasa",
    empty: "Belum ada peristiwa.",
    loading: "Memuat linimasa…",
    sessionExpired: "Sesi berakhir — mengalihkan ke halaman masuk…",
    error: "Gagal memuat peristiwa — gateway mungkin tidak terjangkau.",
    retry: "Coba lagi",
    loadMore: "Muat lebih banyak",
    loadingMore: "Memuat…",
    filterLabel: "Jenis peristiwa",
    filterAll: "Semua jenis",
  },
  requisitionDetail: {
    backToList: "Kembali ke daftar permintaan",
    print: "Cetak",
    notFound: "Permintaan tidak ditemukan (atau milik tenant lain).",
    loadFailed: "Gagal memuat permintaan.",
    fields: {
      requester: "Pemohon",
      currency: "Mata uang",
      total: "Total",
      submitted: "Dikirim",
      created: "Dibuat",
    },
    items: {
      heading: "Item",
      description: "Deskripsi",
      category: "Kategori",
      qty: "Jumlah",
      unitPrice: "Harga satuan",
      lineTotal: "Total baris",
      empty: "Tidak ada item.",
    },
    steps: {
      heading: "Langkah persetujuan",
      step: "Langkah",
      roleRequired: "Peran wajib",
      threshold: "Ambang batas",
      status: "Status",
      decidedBy: "Diputuskan oleh",
      decidedAt: "Diputuskan pada",
      reason: "Alasan",
      empty: "Belum ada langkah persetujuan (terbentuk saat dikirim).",
    },
    attachments: {
      heading: "Lampiran",
      empty: "Tidak ada lampiran.",
    },
    actions: {
      approve: "Setujui",
      reject: "Tolak",
      reasonLabel: "Alasan (wajib untuk menolak)",
      reasonPlaceholder: "Alasan keputusan…",
      reasonRequired: "Penolakan memerlukan alasan.",
      reasonTooLong: "Alasan maksimal 1000 karakter.",
      submitting: "Memutuskan…",
      genericError: "Gagal memutuskan — coba lagi.",
      sessionExpired: "Sesi berakhir — mengalihkan ke halaman masuk…",
      errorMessages: {
        approval_step_not_current: "Langkah ini sudah bukan langkah berjalan (mungkin sudah diputus orang lain).",
        approval_step_not_found: "Langkah persetujuan tidak ditemukan.",
        approval_self_forbidden: "Anda tidak dapat menyetujui permintaan sendiri.",
        approval_role_forbidden: "Langkah ini memerlukan peran yang tidak Anda pegang.",
        approval_decision_invalid: "Keputusan harus setujui atau tolak.",
        approval_reason_required: "Penolakan memerlukan alasan.",
        approval_reason_invalid: "Alasan maksimal 1000 karakter.",
        approval_idempotency_key_invalid: "Kunci idempotensi tidak valid.",
        approval_idempotency_key_conflict: "Kunci idempotensi sudah dipakai untuk langkah lain.",
        gateway_unavailable: "Gateway tidak terjangkau.",
      },
    },
  },
  purchaseOrders: {
    title: "Pesanan Pembelian",
    description:
      "Daftar PO tenant Anda, terbaru dulu. Filter dan paginasi hidup di URL dan disajikan satu halaman per permintaan.",
    caption: "Satu halaman pesanan pembelian",
    unavailable: "Daftar pesanan pembelian tidak tersedia saat ini.",
    empty: "Belum ada pesanan pembelian.",
    emptyHint:
      "PO dibuat dari permintaan yang disetujui (POST /api/v1/purchase-orders/from-requisition/{id} sebagai buyer).",
    columns: {
      number: "Nomor",
      status: "Status",
      vendor: "Pemasok",
      currency: "Mata uang",
      total: "Total",
      issued: "Diterbitkan",
      created: "Dibuat",
    },
    filters: {
      numberLabel: "Cari nomor",
      numberPlaceholder: "PO-000123",
      statusLabel: "Status",
      statusAll: "Semua status",
      pageSizeLabel: "Ukuran halaman",
      apply: "Terapkan",
    },
    pagination: {
      nav: "Navigasi halaman pesanan pembelian",
      previous: "Sebelumnya",
      next: "Berikutnya",
      summary: ({ from, to, total }) => `Menampilkan ${from}–${to} dari ${total.toLocaleString("id-ID")} pesanan pembelian`,
    },
  },
  purchaseOrderDetail: {
    backToList: "Kembali ke daftar pesanan pembelian",
    notFound: "Pesanan pembelian tidak ditemukan (atau milik tenant lain).",
    loadFailed: "Gagal memuat pesanan pembelian.",
    fields: {
      vendor: "Pemasok",
      currency: "Mata uang",
      total: "Total",
      issued: "Diterbitkan",
      created: "Dibuat",
      requisition: "Permintaan asal",
    },
    items: {
      heading: "Item",
      description: "Deskripsi",
      qtyOrdered: "Dipesan",
      qtyReceived: "Diterima",
      remaining: "Sisa",
      unitPrice: "Harga satuan",
      lineTotal: "Total baris",
    },
    actions: {
      issue: "Terbitkan",
      acknowledge: "Konfirmasi pemasok",
      submitting: "Memproses…",
      genericError: "Gagal memproses — coba lagi.",
      sessionExpired: "Sesi berakhir — mengalihkan ke halaman masuk…",
      errorMessages: {
        po_illegal_transition: "PO tidak bisa berpindah ke status itu dari status sekarang.",
        po_has_receipts: "Barang sudah diterima; tutup PO alih-alih membatalkannya.",
        po_not_found: "Pesanan pembelian tidak ditemukan.",
        po_id_invalid: "Id pesanan pembelian harus UUID.",
        gateway_unavailable: "Gateway tidak terjangkau.",
      },
      createInvoice: "Buat faktur",
    },
    receipt: {
      heading: "Catat penerimaan barang (GRN)",
      hint: "Isi jumlah yang diterima per baris — lebih dari 0 dan tidak melebihi sisa. Penerimaan parsial diperbolehkan.",
      qtyLabel: "Diterima sekarang",
      notesLabel: "Catatan (opsional, maks 1000 karakter)",
      notesPlaceholder: "Catatan penerimaan…",
      submit: "Catat penerimaan",
      submitting: "Mencatat…",
      qtyInvalid: "Jumlah harus bilangan bulat lebih dari 0.",
      qtyOver: "Jumlah melebihi sisa baris ini (sisa {remaining}).",
      minOneLine: "Isi minimal satu baris dengan jumlah lebih dari 0.",
      notesTooLong: "Catatan maksimal 1000 karakter.",
      successTitle: "Penerimaan tercatat",
      sessionExpired: "Sesi berakhir — mengalihkan ke halaman masuk…",
      genericError: "Gagal mencatat penerimaan — coba lagi.",
      errorMessages: {
        grn_items_empty: "Penerimaan barang memerlukan minimal satu baris item.",
        grn_qty_invalid: "Setiap jumlah diterima harus lebih dari 0.",
        grn_po_item_duplicate: "Satu baris PO hanya boleh muncul sekali per penerimaan.",
        grn_notes_invalid: "Catatan maksimal 1000 karakter.",
        grn_po_item_unknown: "po_item_id bukan baris dari PO ini.",
        po_id_invalid: "Id pesanan pembelian harus UUID.",
        po_not_found: "Pesanan pembelian tidak ditemukan.",
        po_illegal_transition: "Barang hanya bisa diterima pada PO berstatus acknowledged atau partially_received.",
        gateway_unavailable: "Gateway tidak terjangkau.",
      },
      overReceipt:
        "Kelebihan penerimaan: baris ini sudah diterima {received} dan diminta {requested} lagi, padahal dipesan {ordered}.",
    },
  },
  invoices: {
    title: "Faktur",
    description:
      "Faktur vendor tenant Anda, terbaru dulu. Filter dan paginasi hidup di URL dan disajikan satu halaman per permintaan.",
    caption: "Satu halaman faktur",
    unavailable: "Daftar faktur tidak tersedia saat ini.",
    empty: "Belum ada faktur.",
    emptyHint:
      "Faktur dibuat dari PO yang telah dikonfirmasi (tautan ‘Buat faktur’ di detail PO sebagai finance).",
    columns: {
      number: "Nomor",
      status: "Status",
      vendor: "Pemasok",
      currency: "Mata uang",
      total: "Total",
      created: "Dibuat",
    },
    filters: {
      numberLabel: "Cari nomor",
      numberPlaceholder: "INV/2026/0001",
      statusLabel: "Status",
      statusAll: "Semua status",
      pageSizeLabel: "Ukuran halaman",
      apply: "Terapkan",
    },
    pagination: {
      nav: "Navigasi halaman faktur",
      previous: "Sebelumnya",
      next: "Berikutnya",
      summary: ({ from, to, total }) => `Menampilkan ${from}–${to} dari ${total.toLocaleString("id-ID")} faktur`,
    },
  },
  invoiceDetail: {
    backToList: "Kembali ke daftar faktur",
    notFound: "Faktur tidak ditemukan (atau milik tenant lain).",
    loadFailed: "Gagal memuat faktur.",
    fields: {
      vendor: "Pemasok",
      currency: "Mata uang",
      total: "Total",
      po: "Pesanan pembelian",
      poLink: "Buka pesanan pembelian",
      created: "Dibuat",
    },
    items: {
      heading: "Baris faktur",
      poItem: "Baris PO",
      qty: "Jumlah",
      unitPrice: "Harga satuan",
      lineTotal: "Total baris",
    },
    actions: {
      match: "Jalankan pencocokan",
      submitting: "Mencocokkan…",
      gated: "Unggah dan selesaikan lampiran faktur vendor (status ready) sebelum mencocokkan.",
      genericError: "Gagal menjalankan pencocokan — coba lagi.",
      sessionExpired: "Sesi berakhir — mengalihkan ke halaman masuk…",
      errorMessages: {
        inv_already_matched: "Faktur ini sudah dicocokkan; hasil pencocokan bersifat final.",
        inv_not_found: "Faktur tidak ditemukan.",
        inv_id_invalid: "Id faktur harus UUID.",
        gateway_unavailable: "Gateway tidak terjangkau.",
      },
    },
    attachments: {
      heading: "Lampiran",
      empty: "Belum ada lampiran.",
      columns: {
        filename: "Nama file",
        size: "Ukuran",
        status: "Status",
        download: "Unduh",
      },
      downloadFailed: "Gagal mengambil tautan unduhan — coba lagi.",
      uploadLabel: "Unggah dokumen faktur vendor (pdf, png, jpeg, webp, docx, xlsx)",
      uploading: "Mengunggah…",
    },
    match: {
      heading: "Hasil pencocokan 3-arah",
      verdictMatched: "Cocok",
      verdictDiscrepancy: "Ada selisih",
      pending: "Belum dijalankan — jalankan pencocokan untuk mengisi hasilnya.",
      invalid: "Hasil pencocokan tidak dapat dibaca.",
      columns: {
        poItem: "Baris PO",
        qtyOrdered: "Dipesan",
        qtyReceived: "Diterima",
        qtyInvoiced: "Ditagih",
        qtyDelta: "Δ jumlah",
        poPrice: "Harga PO",
        invoicePrice: "Harga faktur",
        priceDelta: "Δ harga",
        flags: "Penanda",
        reason: "Alasan",
      },
      qtyBreach: "jumlah",
      priceBreach: "harga",
      ok: "sesuai",
      reasonQty: "Kelebihan tagihan jumlah: sudah ditagih {invoiced} padahal diterima {received} (toleransi {pct}%).",
      reasonPrice:
        "Selisih harga: harga PO {poPrice} tetapi faktur menagih {invoicePrice} (toleransi {pct}%).",
    },
  },
  invoiceForm: {
    title: "Faktur baru",
    back: "Kembali",
    missingPO: "Parameter po wajib diisi — buka halaman ini dari detail pesanan pembelian.",
    poNotFound: "Pesanan pembelian tidak ditemukan (atau milik tenant lain).",
    loadFailed: "Gagal memuat pesanan pembelian.",
    notAllowed:
      "Hanya finance/admin yang dapat membuat faktur, dan hanya pada PO berstatus acknowledged, partially_received, atau received.",
    form: {
      heading: "Faktur untuk PO",
      numberLabel: "Nomor faktur vendor (1–100 karakter)",
      numberPlaceholder: "INV/2026/0001",
      itemsHeading: "Baris yang ditagih",
      itemsHint:
        "Isi jumlah per baris PO yang ditagih faktur ini — harga sudah diisi harga PO dan boleh disesuaikan. Baris tanpa jumlah dilewati.",
      itemsLine: "Baris PO",
      itemsQty: "Jumlah",
      itemsUnitPrice: "Harga satuan (sen)",
      itemsLineTotal: "Total baris",
      submit: "Simpan faktur",
      submitting: "Menyimpan…",
      numberInvalid: "Nomor faktur wajib 1–100 karakter setelah dipangkas.",
      qtyInvalid: "Jumlah harus bilangan bulat lebih dari 0.",
      priceInvalid: "Harga satuan harus 0 atau lebih.",
      minOneLine: "Isi minimal satu baris dengan jumlah lebih dari 0.",
      genericError: "Gagal menyimpan faktur — coba lagi.",
      sessionExpired: "Sesi berakhir — mengalihkan ke halaman masuk…",
      errorMessages: {
        inv_number_invalid: "Nomor faktur wajib 1–100 karakter setelah dipangkas.",
        inv_items_empty: "Faktur memerlukan minimal satu baris.",
        inv_qty_invalid: "Setiap jumlah yang ditagih harus lebih dari 0.",
        inv_unit_price_invalid: "Setiap harga satuan harus 0 atau lebih.",
        inv_po_item_duplicate: "Satu baris PO hanya boleh muncul sekali per faktur.",
        inv_po_item_unknown: "po_item_id bukan baris dari pesanan pembelian ini.",
        inv_number_duplicate: "Pemasok ini sudah punya faktur dengan nomor tersebut.",
        po_id_invalid: "Id pesanan pembelian harus UUID.",
        po_not_found: "Pesanan pembelian tidak ditemukan.",
        po_illegal_transition: "Hanya PO acknowledged, partially_received, atau received yang dapat ditagih.",
        gateway_unavailable: "Gateway tidak terjangkau.",
      },
      overBill:
        "Melebihi total PO {poNumber}: sudah ditagih {invoiced} dan faktur ini menagih {requested} lagi, padahal totalnya {total}.",
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
    audit: {
      title: "Log Audit",
      description:
        "Jejak keputusan apa adanya: setiap aksi setuju/tolak/terbit/terima/pencocokan tercatat dengan aktor, IP klien, dan trace.",
      milestone: "M4 — audit",
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
  reportsTable: {
    loading: "Memuat riwayat eksekusi…",
    error: "Gagal memuat riwayat eksekusi — gateway mungkin tidak terjangkau.",
    retry: "Coba lagi",
    sessionExpired: "Sesi berakhir — mengarahkan ke halaman masuk…",
    empty: "Belum ada eksekusi ekspor. Ekspor malam hari pertama berjalan pada jadwal berikutnya.",
    download: "Unduh CSV",
    downloading: "Menyiapkan…",
    downloadFailed: "Gagal menyiapkan tautan unduhan.",
  },
  auditPage: {
    caption: "Log audit keputusan — hanya tambah, tanpa ubah/hapus",
    unavailable: "Gagal memuat log audit — gateway mungkin tidak terjangkau.",
    forbidden: "Log audit memerlukan izin audit:read (viewer atau admin).",
    empty: "Belum ada keputusan tercatat",
    emptyHint: "Setiap aksi setuju/tolak/terbit/terima/pencocokan muncul di sini begitu terjadi.",
    columns: {
      occurredAt: "Waktu",
      actor: "Aktor",
      action: "Aksi",
      entity: "Entitas",
      ip: "IP",
      trace: "Trace",
    },
    filters: { actionLabel: "Aksi", actionAll: "Semua aksi", apply: "Terapkan" },
    pagination: { next: "Berikutnya", first: "Terbaru" },
    exportCsv: "Ekspor CSV",
    exportHint: "Hanya kolom metadata; maksimum 10.000 baris terbaru.",
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
    audit: "Audit Log",
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
    description:
      "The tenant's operational health: the approval queue, spend per vendor, and the purchase-order status distribution.",
    unavailable: "Failed to load the dashboard summary — is the gateway running and the access token valid?",
    kpi: {
      pending: "Pending approvals",
      spend30: "30-day spend",
      spend90: "90-day spend",
      poTotal: "Purchase orders",
      pendingNote: "current pending approval steps",
      spend30Note: "POs issued in the last 30 days",
      spend90Note: "POs issued in the last 90 days",
      poTotalNote: "across all FR-06 statuses",
    },
    approvals: {
      heading: "Pending approvals",
      caption: "Pending approval queue, oldest submission first",
      empty: "No pending approvals.",
      hidden: (hidden) => `+ ${hidden.toLocaleString("en-US")} more in the queue`,
      columns: {
        number: "Number",
        submitted: "Submitted",
        total: "Total",
        step: "Step",
        role: "Approver role",
      },
    },
    spend: {
      heading: "Spend by vendor",
      caption: "Issued PO totals per vendor and currency",
      empty: "No issued-PO spend in the last 90 days yet.",
      columns: { vendor: "Vendor", thirty: "30 days", ninety: "90 days" },
    },
    distribution: {
      heading: "PO status distribution",
      caption: "Purchase-order counts per FR-06 status",
      total: "Total",
    },
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
  requisitionForm: {
    title: "New purchase requisition",
    description:
      "Four steps: details → items → attachments → review. The draft autosaves to the server and is restored on reload.",
    newLink: "New requisition",
    steps: { details: "Details", items: "Items", attachments: "Attachments", review: "Review" },
    details: {
      heading: "Requisition details",
      currencyLabel: "Currency",
      currencyPlaceholder: "Choose a currency…",
      currencyLockedNote:
        "Currency is locked once the draft is created (attachments are bound to this draft). To change it, start a new draft.",
      next: "Continue to items",
    },
    items: {
      heading: "Requisition items",
      description: "Description",
      qty: "Qty",
      unitPrice: "Unit price (cents)",
      category: "Category",
      add: "Add item",
      remove: "Remove item",
      next: "Continue to attachments",
      back: "Back",
    },
    attachments: {
      heading: "Attachments",
      hint: "Files upload directly to storage via a presigned URL (pdf, png, jpeg, webp, docx, xlsx; max 25 MB).",
      add: "Choose a file",
      empty: "No attachments yet.",
      uploading: "Uploading…",
      next: "Continue to review",
      back: "Back",
    },
    review: {
      heading: "Review and submit",
      itemsHeading: "Items",
      attachmentsHeading: "Attachments",
      total: "Total (cents)",
      submit: "Submit for approval",
      submitting: "Submitting…",
      back: "Back",
      itemsEmpty: "Add at least one item before submitting.",
    },
    autosave: { saving: "Saving draft…", saved: "Draft saved.", failed: "Failed to save the draft." },
    restore: {
      loading: "Restoring draft…",
      notFound: "Draft not found (or it belongs to another tenant).",
      error: "Failed to load the draft.",
      notDraft:
        "This requisition is no longer a draft (it may have been submitted from another tab) — the form is read-only and autosave stopped.",
    },
    submitted: { heading: "Requisition submitted:", backToList: "Back to the requisition list" },
  },
  timeline: {
    title: "Timeline",
    empty: "No events yet.",
    loading: "Loading timeline…",
    sessionExpired: "Session expired — redirecting to sign-in…",
    error: "Failed to load events — the gateway may be unreachable.",
    retry: "Retry",
    loadMore: "Load more",
    loadingMore: "Loading…",
    filterLabel: "Event type",
    filterAll: "All types",
  },
  requisitionDetail: {
    backToList: "Back to the requisition list",
    print: "Print",
    notFound: "Requisition not found (or it belongs to another tenant).",
    loadFailed: "Failed to load the requisition.",
    fields: {
      requester: "Requester",
      currency: "Currency",
      total: "Total",
      submitted: "Submitted",
      created: "Created",
    },
    items: {
      heading: "Items",
      description: "Description",
      category: "Category",
      qty: "Qty",
      unitPrice: "Unit price",
      lineTotal: "Line total",
      empty: "No items.",
    },
    steps: {
      heading: "Approval steps",
      step: "Step",
      roleRequired: "Role required",
      threshold: "Threshold",
      status: "Status",
      decidedBy: "Decided by",
      decidedAt: "Decided at",
      reason: "Reason",
      empty: "No approval steps yet (they are created on submit).",
    },
    attachments: {
      heading: "Attachments",
      empty: "No attachments.",
    },
    actions: {
      approve: "Approve",
      reject: "Reject",
      reasonLabel: "Reason (required to reject)",
      reasonPlaceholder: "Reason for the decision…",
      reasonRequired: "A reject requires a reason.",
      reasonTooLong: "Reason must be at most 1000 characters.",
      submitting: "Deciding…",
      genericError: "Failed to decide — try again.",
      sessionExpired: "Session expired — redirecting to sign-in…",
      errorMessages: {
        approval_step_not_current: "This step is no longer the current pending step (someone may have decided it).",
        approval_step_not_found: "Approval step not found.",
        approval_self_forbidden: "You cannot decide your own requisition.",
        approval_role_forbidden: "This step requires a role you do not hold.",
        approval_decision_invalid: "Decision must be approve or reject.",
        approval_reason_required: "A reject requires a reason.",
        approval_reason_invalid: "Reason must be at most 1000 characters.",
        approval_idempotency_key_invalid: "Invalid idempotency key.",
        approval_idempotency_key_conflict: "The idempotency key was already used for another step.",
        gateway_unavailable: "The gateway is unreachable.",
      },
    },
  },
  purchaseOrders: {
    title: "Purchase Orders",
    description:
      "Your tenant's purchase orders, newest first. Filters and pagination live in the URL and are served one page at a time.",
    caption: "One page of purchase orders",
    unavailable: "The purchase order list is unavailable right now.",
    empty: "No purchase orders yet.",
    emptyHint:
      "POs are created from approved requisitions (POST /api/v1/purchase-orders/from-requisition/{id} as a buyer).",
    columns: {
      number: "Number",
      status: "Status",
      vendor: "Vendor",
      currency: "Currency",
      total: "Total",
      issued: "Issued",
      created: "Created",
    },
    filters: {
      numberLabel: "Search number",
      numberPlaceholder: "PO-000123",
      statusLabel: "Status",
      statusAll: "All statuses",
      pageSizeLabel: "Page size",
      apply: "Apply",
    },
    pagination: {
      nav: "Purchase order page navigation",
      previous: "Previous",
      next: "Next",
      summary: ({ from, to, total }) => `Showing ${from}–${to} of ${total.toLocaleString("en-US")} purchase orders`,
    },
  },
  purchaseOrderDetail: {
    backToList: "Back to the purchase order list",
    notFound: "Purchase order not found (or it belongs to another tenant).",
    loadFailed: "Failed to load the purchase order.",
    fields: {
      vendor: "Vendor",
      currency: "Currency",
      total: "Total",
      issued: "Issued",
      created: "Created",
      requisition: "Source requisition",
    },
    items: {
      heading: "Items",
      description: "Description",
      qtyOrdered: "Ordered",
      qtyReceived: "Received",
      remaining: "Remaining",
      unitPrice: "Unit price",
      lineTotal: "Line total",
    },
    actions: {
      issue: "Issue",
      acknowledge: "Acknowledge vendor",
      submitting: "Working…",
      genericError: "Failed — try again.",
      sessionExpired: "Session expired — redirecting to sign-in…",
      errorMessages: {
        po_illegal_transition: "The purchase order cannot move that way from its current status.",
        po_has_receipts: "Goods were already received; close the purchase order instead of cancelling it.",
        po_not_found: "Purchase order not found.",
        po_id_invalid: "Purchase order id must be a UUID.",
        gateway_unavailable: "The gateway is unreachable.",
      },
      createInvoice: "Create invoice",
    },
    receipt: {
      heading: "Record a goods receipt (GRN)",
      hint: "Enter what actually arrived per line — more than 0 and never above the remaining quantity. Partial receipts are allowed.",
      qtyLabel: "Received now",
      notesLabel: "Notes (optional, max 1000 characters)",
      notesPlaceholder: "Receiving notes…",
      submit: "Record receipt",
      submitting: "Recording…",
      qtyInvalid: "Quantity must be a whole number greater than 0.",
      qtyOver: "Quantity exceeds this line's remaining {remaining}.",
      minOneLine: "Fill at least one line with a quantity greater than 0.",
      notesTooLong: "Notes are limited to 1000 characters.",
      successTitle: "Receipt recorded",
      sessionExpired: "Session expired — redirecting to sign-in…",
      genericError: "Failed to record the receipt — try again.",
      errorMessages: {
        grn_items_empty: "A goods receipt needs at least one item line.",
        grn_qty_invalid: "Each received quantity must be greater than zero.",
        grn_po_item_duplicate: "Each purchase order line may appear once per goods receipt.",
        grn_notes_invalid: "Notes are limited to 1000 characters.",
        grn_po_item_unknown: "po_item_id does not name a line of this purchase order.",
        po_id_invalid: "Purchase order id must be a UUID.",
        po_not_found: "Purchase order not found.",
        po_illegal_transition: "Goods can only be received on an acknowledged or partially received purchase order.",
        gateway_unavailable: "The gateway is unreachable.",
      },
      overReceipt:
        "Over-receipt: this line has already received {received} and requests {requested} more, but only {ordered} were ordered.",
    },
  },
  invoices: {
    title: "Invoices",
    description:
      "Your tenant's vendor invoices, newest first. Filters and pagination live in the URL and are served one page at a time.",
    caption: "One page of invoices",
    unavailable: "The invoice list is unavailable right now.",
    empty: "No invoices yet.",
    emptyHint:
      "Invoices are created from confirmed purchase orders (the ‘Create invoice’ link on a PO detail, as finance).",
    columns: {
      number: "Number",
      status: "Status",
      vendor: "Vendor",
      currency: "Currency",
      total: "Total",
      created: "Created",
    },
    filters: {
      numberLabel: "Search number",
      numberPlaceholder: "INV/2026/0001",
      statusLabel: "Status",
      statusAll: "All statuses",
      pageSizeLabel: "Page size",
      apply: "Apply",
    },
    pagination: {
      nav: "Invoice page navigation",
      previous: "Previous",
      next: "Next",
      summary: ({ from, to, total }) => `Showing ${from}–${to} of ${total.toLocaleString("en-US")} invoices`,
    },
  },
  invoiceDetail: {
    backToList: "Back to the invoice list",
    notFound: "Invoice not found (or it belongs to another tenant).",
    loadFailed: "Failed to load the invoice.",
    fields: {
      vendor: "Vendor",
      currency: "Currency",
      total: "Total",
      po: "Purchase order",
      poLink: "Open purchase order",
      created: "Created",
    },
    items: {
      heading: "Invoice lines",
      poItem: "PO line",
      qty: "Qty",
      unitPrice: "Unit price",
      lineTotal: "Line total",
    },
    actions: {
      match: "Run match",
      submitting: "Matching…",
      gated: "Upload and complete a vendor invoice attachment (ready status) before matching.",
      genericError: "Failed to run the match — try again.",
      sessionExpired: "Session expired — redirecting to sign-in…",
      errorMessages: {
        inv_already_matched: "This invoice is already matched; the result is final.",
        inv_not_found: "Invoice not found.",
        inv_id_invalid: "Invoice id must be a UUID.",
        gateway_unavailable: "The gateway is unreachable.",
      },
    },
    attachments: {
      heading: "Attachments",
      empty: "No attachments yet.",
      columns: {
        filename: "Filename",
        size: "Size",
        status: "Status",
        download: "Download",
      },
      downloadFailed: "Could not fetch the download link — try again.",
      uploadLabel: "Upload the vendor invoice document (pdf, png, jpeg, webp, docx, xlsx)",
      uploading: "Uploading…",
    },
    match: {
      heading: "Three-way match result",
      verdictMatched: "Matched",
      verdictDiscrepancy: "Discrepancy",
      pending: "Not run yet — run the match to fill in the result.",
      invalid: "The stored match result could not be read.",
      columns: {
        poItem: "PO line",
        qtyOrdered: "Ordered",
        qtyReceived: "Received",
        qtyInvoiced: "Invoiced",
        qtyDelta: "Δ qty",
        poPrice: "PO price",
        invoicePrice: "Invoice price",
        priceDelta: "Δ price",
        flags: "Flags",
        reason: "Reason",
      },
      qtyBreach: "qty",
      priceBreach: "price",
      ok: "ok",
      reasonQty: "Quantity over-billed: {invoiced} invoiced while only {received} were received (tolerance {pct}%).",
      reasonPrice: "Price differs: the PO price is {poPrice} but the invoice bills {invoicePrice} (tolerance {pct}%).",
    },
  },
  invoiceForm: {
    title: "New invoice",
    back: "Back",
    missingPO: "The po parameter is required — open this page from a purchase order detail.",
    poNotFound: "Purchase order not found (or it belongs to another tenant).",
    loadFailed: "Failed to load the purchase order.",
    notAllowed:
      "Only finance/admin can create invoices, and only on an acknowledged, partially_received, or received PO.",
    form: {
      heading: "Invoice for PO",
      numberLabel: "Vendor invoice number (1–100 characters)",
      numberPlaceholder: "INV/2026/0001",
      itemsHeading: "Lines to bill",
      itemsHint:
        "Fill the quantity for each PO line this invoice bills — prices are prefilled with the PO price and may be adjusted. Lines without a quantity are skipped.",
      itemsLine: "PO line",
      itemsQty: "Qty",
      itemsUnitPrice: "Unit price (cents)",
      itemsLineTotal: "Line total",
      submit: "Save invoice",
      submitting: "Saving…",
      numberInvalid: "The invoice number must be 1–100 characters after trimming.",
      qtyInvalid: "Quantity must be a whole number greater than 0.",
      priceInvalid: "Unit price must be zero or more.",
      minOneLine: "Fill at least one line with a quantity greater than 0.",
      genericError: "Failed to save the invoice — try again.",
      sessionExpired: "Session expired — redirecting to sign-in…",
      errorMessages: {
        inv_number_invalid: "The invoice number must be 1–100 characters after trimming.",
        inv_items_empty: "An invoice needs at least one item line.",
        inv_qty_invalid: "Each invoiced quantity must be greater than zero.",
        inv_unit_price_invalid: "Each invoiced unit price must be zero or more.",
        inv_po_item_duplicate: "Each purchase order line may appear once per invoice.",
        inv_po_item_unknown: "po_item_id does not name a line of this purchase order.",
        inv_number_duplicate: "This vendor already has an invoice with this number.",
        po_id_invalid: "Purchase order id must be a UUID.",
        po_not_found: "Purchase order not found.",
        po_illegal_transition: "Only acknowledged, partially received, or received purchase orders can be invoiced.",
        gateway_unavailable: "The gateway is unreachable.",
      },
      overBill:
        "Over-bills PO {poNumber}: {invoiced} already invoiced and this invoice requests {requested} more, but the total is {total}.",
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
    audit: {
      title: "Audit Log",
      description:
        "The decision trail as it happened: every approve/reject/issue/post/match action with its actor, client IP, and trace.",
      milestone: "M4 — audit",
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
  reportsTable: {
    loading: "Loading run history…",
    error: "Failed to load run history — the gateway may be unreachable.",
    retry: "Retry",
    sessionExpired: "Session expired — redirecting to sign-in…",
    empty: "No export runs yet. The first nightly export runs on the next schedule.",
    download: "Download CSV",
    downloading: "Preparing…",
    downloadFailed: "Failed to prepare the download link.",
  },
  auditPage: {
    caption: "Decision audit log — append-only, no edits or deletes",
    unavailable: "Failed to load the audit log — the gateway may be unreachable.",
    forbidden: "The audit log requires the audit:read permission (viewer or admin).",
    empty: "No decisions recorded yet",
    emptyHint: "Every approve/reject/issue/post/match action appears here as it happens.",
    columns: {
      occurredAt: "When",
      actor: "Actor",
      action: "Action",
      entity: "Entity",
      ip: "IP",
      trace: "Trace",
    },
    filters: { actionLabel: "Action", actionAll: "All actions", apply: "Apply" },
    pagination: { next: "Older", first: "Newest" },
    exportCsv: "Export CSV",
    exportHint: "Metadata columns only; the newest 10,000 rows at most.",
  },
  localeNames: { id: "Bahasa Indonesia", en: "English" },
};

export const dictionaries: Record<Locale, Dictionary> = { id, en };

export function getDictionary(locale: Locale): Dictionary {
  return dictionaries[locale];
}
