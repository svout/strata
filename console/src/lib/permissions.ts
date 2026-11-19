import type { Role } from "@/api/types";

/**
 * Who can do what in a workspace (P10 board → "What each role can do").
 *
 * Roles are for people. Agents never have a role: what an agent may do is
 * set by its mandates and its keys, not by this table.
 *
 * The server enforces these; the console only explains them, by showing a
 * control disabled with a tooltip that names who can use it, so nobody is
 * offered an action that will be refused and nobody wonders where a
 * feature went.
 */
export type Action =
  | "view"
  | "review" // approve or decline in the review queue
  | "editMandates" // create, edit, pause, resume
  | "manageAgents" // register, pause, resume
  | "manageKeys" // create, revoke
  | "manageMembers" // invite, change role, remove
  | "renameWorkspace"
  | "deleteWorkspace"
  | "runDemo"; // attack lab, simulated traffic, tamper demo

export const ACTIONS: Record<Action, { label: string; verb: string; group: string }> = {
  view: { label: "See every decision, the ledger and all settings", verb: "see this", group: "Everyone" },
  review: { label: "Approve or decline held payments", verb: "approve or decline payments", group: "Daily work" },
  editMandates: { label: "Create, edit and pause mandates", verb: "change mandates", group: "Spending rules" },
  manageAgents: { label: "Register, pause and resume agents", verb: "register or pause agents", group: "Agents and keys" },
  manageKeys: { label: "Create and revoke API keys", verb: "create or revoke keys", group: "Agents and keys" },
  manageMembers: { label: "Invite people, change roles, remove people", verb: "manage members", group: "Workspace" },
  renameWorkspace: { label: "Rename the workspace", verb: "rename the workspace", group: "Workspace" },
  deleteWorkspace: { label: "Delete the workspace", verb: "delete the workspace", group: "Workspace" },
  runDemo: { label: "Run the attack lab and demo tools", verb: "run demos", group: "Demo" },
};

const ALLOWED: Record<Role, Action[]> = {
  owner: ["view", "review", "editMandates", "manageAgents", "manageKeys", "manageMembers", "renameWorkspace", "deleteWorkspace", "runDemo"],
  admin: ["view", "review", "editMandates", "manageAgents", "manageKeys", "manageMembers", "runDemo"],
  member: ["view", "review", "editMandates", "runDemo"],
  viewer: ["view"],
};

export const ROLES: Role[] = ["owner", "admin", "member", "viewer"];
export const can = (role: Role, action: Action) => ALLOWED[role].includes(action);

const LABEL: Record<Role, string> = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Viewer" };
const PLURAL: Record<Role, string> = { owner: "owners", admin: "admins", member: "members", viewer: "viewers" };

/** "Owners and admins" — who may do this, for tooltips. */
export function whoCan(action: Action): string {
  const roles = ROLES.filter((r) => can(r, action)).map((r) => PLURAL[r]);
  const text = roles.length <= 1 ? roles[0] ?? "nobody" : `${roles.slice(0, -1).join(", ")} and ${roles[roles.length - 1]}`;
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export function deniedReason(action: Action, role: Role): string {
  return `${whoCan(action)} can ${ACTIONS[action].verb}. You're signed in as ${role === "admin" || role === "owner" ? "an" : "a"} ${LABEL[role]}.`;
}

export const ROLE_INFO: Record<Role, { label: string; text: string; who: string }> = {
  owner: {
    label: "Owner",
    text: "Everything, including deleting the workspace. Every workspace keeps at least one.",
    who: "The person accountable for the account: usually the founder or head of finance.",
  },
  admin: {
    label: "Admin",
    text: "Runs the setup: members, agents, keys and mandates. Can't delete the workspace.",
    who: "Whoever connects agents and sets things up: an engineering lead or ops manager.",
  },
  member: {
    label: "Member",
    text: "Day-to-day work: approves or declines held payments and edits mandates. Can't touch agents, keys or people.",
    who: "Finance or ops staff who clear the review queue.",
  },
  viewer: {
    label: "Viewer",
    text: "Reads everything and changes nothing.",
    who: "Auditors, accountants, a supervisor or an investor looking over your shoulder.",
  },
};
