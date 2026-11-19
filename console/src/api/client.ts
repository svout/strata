import type {
  Agent,
  AgentDay,
  ApiKey,
  ChainStatus,
  CreatedKey,
  Decision,
  DecisionFilter,
  DeclineReason,
  Dispute,
  Invite,
  LedgerEntry,
  LedgerEventType,
  Mandate,
  MandateInput,
  MandateVersion,
  Member,
  Merchant,
  Overview,
  OverviewRange,
  ReplayResult,
  Role,
  Scenario,
  Webhook,
  Workspace,
} from "./types";

/**
 * Everything the console asks of the platform. One interface, two
 * implementations: the mock gateway (src/api/mock), used today, and the
 * generated HTTP client for the control-plane API, later. Screens only see
 * this interface, so swapping is a one-line change in src/api/index.ts.
 *
 * Reads are what the console mostly does. Writes are limited to what an
 * operator is allowed to change: reviews, mandates, agents and keys,
 * members. Decisions and ledger entries are never edited.
 */
export interface ConsoleApi {
  // Monitor
  overview(range: OverviewRange): Promise<Overview>;
  decisions(filter: DecisionFilter): Promise<Decision[]>;
  decision(id: string): Promise<Decision | null>;
  reviewQueue(): Promise<Decision[]>;
  approve(id: string, actor: string): Promise<Decision>;
  decline(id: string, actor: string, reason: DeclineReason): Promise<Decision>;
  dispute(decisionId: string): Promise<Dispute | null>;

  // Control
  merchants(): Promise<Merchant[]>;
  mandates(): Promise<Mandate[]>;
  mandate(id: string): Promise<Mandate | null>;
  mandateHistory(id: string): Promise<MandateVersion[]>;
  saveMandate(id: string | null, input: MandateInput, actor: string): Promise<Mandate>;
  setMandateStatus(id: string, status: Mandate["status"], actor: string): Promise<Mandate>;
  replayMandate(id: string | null, input: MandateInput, days: number): Promise<ReplayResult>;
  agents(): Promise<Agent[]>;
  agentActivity(agentId: string, days: number): Promise<AgentDay[]>;
  registerAgent(input: { id: string; description: string; owner: string }, actor: string): Promise<{ agent: Agent; created: CreatedKey }>;
  setAgentStatus(id: string, status: Agent["status"], actor: string): Promise<Agent>;
  keys(agentId?: string): Promise<ApiKey[]>;
  createKey(agentId: string, mode: ApiKey["mode"], actor: string): Promise<CreatedKey>;
  revokeKey(keyId: string, actor: string): Promise<ApiKey>;

  // Record
  ledger(filter: { events?: LedgerEventType[]; since?: number }): Promise<LedgerEntry[]>;
  verifyChain(): Promise<ChainStatus>;
  lastVerification(): Promise<ChainStatus>;

  // Admin
  workspace(): Promise<Workspace>;
  renameWorkspace(name: string, actor: string): Promise<Workspace>;
  members(): Promise<Member[]>;
  invites(): Promise<Invite[]>;
  invite(email: string, role: Invite["role"], actor: string): Promise<Invite>;
  revokeInvite(id: string, actor: string): Promise<void>;
  setRole(memberId: string, role: Role, actor: string): Promise<Member>;
  removeMember(memberId: string, actor: string): Promise<void>;
  webhooks(): Promise<Webhook[]>;

  // Demo (development and the viva only; not part of the real API)
  scenarios(): Promise<Scenario[]>;
  runScenario(n: number): Promise<Decision>;
  simulateTraffic(): Promise<Decision>;
  /** Onboarding step 4: one small payment from a new agent, inside its first mandate. */
  testPayment(agentId: string): Promise<Decision>;
  tamperWithEntry(): Promise<number>;
  restoreTamperedEntry(): Promise<void>;
  reset(): Promise<void>;

  /** Called after any change, so open screens can refetch. */
  subscribe(listener: () => void): () => void;
}
