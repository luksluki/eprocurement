import {
  createTable,
  getCoreRowModel,
  type ColumnDef,
  type RowData,
  type TableState,
} from "@tanstack/react-table";
import type { ReactNode } from "react";
import { Card } from "../components/Card";

export type Align = "left" | "right" | "center";

// Opinionated column API: the app never touches @tanstack/react-table types.
// key identifies the column (and the default cell value), label is the header
// content, render overrides the default `String(row[key])`, align maps to a
// text-align class.
export type DataTableColumn<T> = {
  key: string;
  label: ReactNode;
  render?: (row: T) => ReactNode;
  align?: Align;
};

export type DataTableProps<T extends RowData> = {
  data: T[];
  columns: Array<DataTableColumn<T>>;
  getRowId: (row: T) => string;
  caption?: string;
  emptyState?: ReactNode;
};

const ALIGN_CLASSES: Record<Align, string> = {
  left: "text-left",
  right: "text-right",
  center: "text-center",
};

const HEADER_CLASSES = "px-4 py-3 text-xs font-medium uppercase tracking-wide text-ink-subtle";
const CELL_CLASSES = "px-4 py-3";

// Server-mode table on TanStack Table v8: the caller fetches one page slice
// and drives paging through links/URLs (manualPagination: true, see
// TablePagination). It renders entirely on the server via the
// framework-agnostic core API (createTable + getCoreRowModel), so it adds no
// client bundle.
export function DataTable<T extends RowData>({
  data,
  columns,
  getRowId,
  caption,
  emptyState,
}: DataTableProps<T>) {
  const columnDefs: ColumnDef<T>[] = columns.map((column) => ({
    id: column.key,
    accessorFn: (row) => (row as Record<string, unknown>)[column.key],
  }));

  const table = createTable<T>({
    data,
    columns: columnDefs,
    getCoreRowModel: getCoreRowModel(),
    getRowId: (row) => getRowId(row),
    manualPagination: true,
    pageCount: -1,
    // TableOptionsResolved demands the three keys below; the adapter pattern
    // (see useReactTable) is to create with dummy values, then swap in the
    // feature-initialized state. No stateful feature (pagination/filters/
    // sorting) is mounted on this read-only server table.
    state: {} as TableState,
    onStateChange: () => undefined,
    renderFallbackValue: null,
  });
  table.setOptions((prev) => ({ ...prev, state: table.initialState }));

  if (data.length === 0 && emptyState !== undefined) {
    return <Card className="overflow-x-auto">{emptyState}</Card>;
  }

  const headerGroups = table.getHeaderGroups();
  const rows = table.getRowModel().rows;
  const columnByKey = new Map(columns.map((column) => [column.key, column]));

  return (
    <Card className="overflow-x-auto">
      <table className="w-full text-left text-sm text-ink">
        {caption ? <caption className="sr-only">{caption}</caption> : null}
        <thead className="border-b border-line">
          {headerGroups.map((headerGroup) => (
            <tr key={headerGroup.id}>
              {headerGroup.headers.map((header) => {
                const column = columnByKey.get(header.column.id);
                if (!column) {
                  return null;
                }
                return (
                  <th
                    key={header.id}
                    scope="col"
                    className={`${HEADER_CLASSES} ${ALIGN_CLASSES[column.align ?? "left"]}`}
                  >
                    {column.label}
                  </th>
                );
              })}
            </tr>
          ))}
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.id} className="border-b border-line-soft last:border-0">
              {headerGroups[0].headers.map((header) => {
                const column = columnByKey.get(header.column.id);
                if (!column) {
                  return null;
                }
                const cell = column.render
                  ? column.render(row.original)
                  : String((row.original as Record<string, unknown>)[column.key] ?? "");
                return (
                  <td
                    key={`${row.id}:${header.column.id}`}
                    className={`${CELL_CLASSES} ${ALIGN_CLASSES[column.align ?? "left"]}`}
                  >
                    {cell}
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  );
}
