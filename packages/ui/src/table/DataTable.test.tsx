import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { DataTable, type DataTableColumn } from "./DataTable";

type Row = {
  id: string;
  code: string;
  name: string;
  status: string;
  amount: number;
};

// Server-mode proof at the 100k scale required by PRD §19 (p95 on a 100k-item
// seed): the table only ever receives the current page slice — this stub
// claims total=100_000 with pageSize=50 and asserts DataTable renders the
// requested slice and nothing else. The live tenant holds only 2–3 vendors
// (dev note 050), so the 100k claim is proven by this stub, not by seeded
// data; see dev note 051.
const TOTAL = 100_000;
const PAGE_SIZE = 50;

function makeRows(page: number): Row[] {
  const first = (page - 1) * PAGE_SIZE + 1;
  return Array.from({ length: PAGE_SIZE }, (_, i) => {
    const n = first + i;
    return {
      id: `row-${n}`,
      code: `V-${String(n).padStart(6, "0")}`,
      name: `Vendor ${n}`,
      status: n % 2 === 0 ? "active" : "deactivated",
      amount: n * 10,
    };
  });
}

const columns: DataTableColumn<Row>[] = [
  { key: "code", label: "Code" },
  { key: "name", label: "Name" },
  { key: "amount", label: "Amount", align: "right", render: (row) => row.amount.toLocaleString("en-US") },
  { key: "status", label: "Status", render: (row) => row.status.toUpperCase() },
];

afterEach(() => {
  cleanup();
});

describe("DataTable (server mode, stub total=100000)", () => {
  it("renders only the requested page slice — 50 of 100000 rows", () => {
    render(<DataTable data={makeRows(3)} columns={columns} getRowId={(row) => row.id} caption="Vendors" />);

    // First and last row of page 3 are present…
    expect(screen.getByText("V-000101")).toBeTruthy();
    expect(screen.getByText("V-000150")).toBeTruthy();
    // …while neighbouring pages are never rendered.
    expect(screen.queryByText("V-000100")).toBeNull();
    expect(screen.queryByText("V-000151")).toBeNull();
    expect(screen.queryByText("V-000001")).toBeNull();
    // 50 data rows exactly, and the last slice of the stub ends at V-100000.
    expect(document.querySelectorAll("tbody tr")).toHaveLength(PAGE_SIZE);
    const lastSlice = makeRows(TOTAL / PAGE_SIZE);
    expect(lastSlice.at(-1)?.code).toBe("V-100000");
  });

  it("renders column headers with scope=col and a screen-reader caption", () => {
    render(<DataTable data={makeRows(1)} columns={columns} getRowId={(row) => row.id} caption="Vendor list" />);
    expect(screen.getByRole("columnheader", { name: "Code" }).getAttribute("scope")).toBe("col");
    expect(screen.getByText("Vendor list").tagName).toBe("CAPTION");
  });

  it("applies render overrides and alignment classes", () => {
    render(<DataTable data={makeRows(1)} columns={columns} getRowId={(row) => row.id} />);
    // Render override uppercases status; the 50-row slice has 25 of each.
    expect(screen.getAllByText("ACTIVE")).toHaveLength(25);
    expect(screen.getAllByText("DEACTIVATED")).toHaveLength(25);
    const amountCell = screen.getByText("30").closest("td"); // row 3 amount
    expect(amountCell?.className).toContain("text-right");
  });

  it("falls back to String(row[key]) for columns without render", () => {
    render(<DataTable data={makeRows(1)} columns={columns} getRowId={(row) => row.id} />);
    expect(screen.getByText("Vendor 1")).toBeTruthy();
  });

  it("renders the empty state instead of a table when the page is empty (failure path)", () => {
    render(
      <DataTable data={[]} columns={columns} getRowId={(row) => row.id} emptyState={<p>No vendors yet.</p>} />,
    );
    expect(screen.getByText("No vendors yet.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });
});
