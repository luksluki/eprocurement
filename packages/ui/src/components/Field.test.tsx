import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Field } from "./Field";

afterEach(() => {
  cleanup();
});

describe("Field (input)", () => {
  it("binds the label to the control and forwards the value", () => {
    render(<Field id="email" label="Email" type="email" value="a@b.c" onValueChange={() => undefined} />);
    const input = screen.getByLabelText("Email") as HTMLInputElement;
    expect(input.id).toBe("email");
    expect(input.value).toBe("a@b.c");
    expect(input.getAttribute("type")).toBe("email");
  });

  it("emits onValueChange on typing", () => {
    const onValueChange = vi.fn();
    render(<Field id="email" label="Email" value="" onValueChange={onValueChange} />);
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "admin@acme.example" } });
    expect(onValueChange).toHaveBeenCalledWith("admin@acme.example");
  });

  it("announces errors via role=alert and aria-describedby (failure path)", () => {
    render(<Field id="email" label="Email" value="" error="Enter a valid email." />);
    const input = screen.getByLabelText("Email");
    expect(input.getAttribute("aria-describedby")).toBe("email-error");
    expect(screen.getByRole("alert").textContent).toBe("Enter a valid email.");
  });

  it("links hints alongside errors when both are present", () => {
    render(<Field id="code" label="Code" value="" hint="Uppercase" error="Required." />);
    expect(screen.getByLabelText("Code").getAttribute("aria-describedby")).toBe("code-hint code-error");
  });
});

describe("Field (select)", () => {
  const options = [
    { value: "acme", label: "Acme" },
    { value: "glow", label: "Glow" },
  ];

  it("renders label + options with the controlled value", () => {
    render(<Field id="tenant" control="select" label="Tenant" value="acme" options={options} />);
    const select = screen.getByLabelText("Tenant") as HTMLSelectElement;
    expect(select.value).toBe("acme");
    expect(screen.getByRole("option", { name: "Glow" }).getAttribute("value")).toBe("glow");
  });

  it("emits onValueChange on change", () => {
    const onValueChange = vi.fn();
    render(
      <Field id="tenant" control="select" label="Tenant" value="" options={options} onValueChange={onValueChange} />,
    );
    fireEvent.change(screen.getByLabelText("Tenant"), { target: { value: "glow" } });
    expect(onValueChange).toHaveBeenCalledWith("glow");
  });

  it("renders a disabled placeholder option for the empty value (failure path)", () => {
    render(
      <Field
        id="tenant"
        control="select"
        label="Tenant"
        value=""
        placeholderOption="Pick one"
        options={options}
      />,
    );
    const placeholder = screen.getByRole("option", { name: "Pick one" }) as HTMLOptionElement;
    expect(placeholder.disabled).toBe(true);
  });
});
