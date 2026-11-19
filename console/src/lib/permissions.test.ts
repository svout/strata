import { describe, expect, it } from "vitest";
import { ACTIONS, ROLES, can, deniedReason, whoCan, type Action } from "./permissions";

describe("roles", () => {
  it("matches the P10 split, from the most to the least power", () => {
    const table = Object.fromEntries(ROLES.map((r) => [r, (Object.keys(ACTIONS) as Action[]).filter((a) => can(r, a))]));
    expect(table).toEqual({
      owner: ["view", "review", "editMandates", "manageAgents", "manageKeys", "manageMembers", "renameWorkspace", "deleteWorkspace", "runDemo"],
      admin: ["view", "review", "editMandates", "manageAgents", "manageKeys", "manageMembers", "runDemo"],
      member: ["view", "review", "editMandates", "runDemo"],
      viewer: ["view"],
    });
  });

  it("each role can do everything the role below it can", () => {
    for (let i = 1; i < ROLES.length; i++) {
      for (const a of Object.keys(ACTIONS) as Action[]) {
        if (can(ROLES[i], a)) expect(can(ROLES[i - 1], a)).toBe(true);
      }
    }
  });

  it("explains a refusal in words", () => {
    expect(whoCan("manageAgents")).toBe("Owners and admins");
    expect(deniedReason("review", "viewer")).toBe("Owners, admins and members can approve or decline payments. You're signed in as a Viewer.");
    expect(deniedReason("deleteWorkspace", "admin")).toBe("Owners can delete the workspace. You're signed in as an Admin.");
  });
});
