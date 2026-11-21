// Human-readable ABIs for AgentPayRouter (contracts/contracts/AgentPayRouter.sol) and an ERC-20.
export const ROUTER_ABI = [
  "function token() view returns (address)",
  "function invoicePaid(address merchant, bytes32 invoiceId) view returns (bool)",
  "function mandate(address owner, address agent) view returns (tuple(uint128 perPaymentCap, uint128 dailyCap, uint128 remainingToday, uint64 expiresAt, bool active, bool merchantLocked))",
  "function isMerchantAllowed(address owner, address agent, address merchant) view returns (bool)",
  "function grantMandate(address agent, uint128 perPaymentCap, uint128 dailyCap, uint64 expiresAt, address[] merchants)",
  "function revokeMandate(address agent)",
  "function pay(bytes32 invoiceId, address merchant, uint256 amount)",
  "function payAsAgent(address owner, bytes32 invoiceId, address merchant, uint256 amount)",
  "event MandateGranted(address indexed owner, address indexed agent, uint128 perPaymentCap, uint128 dailyCap, uint64 expiresAt, address[] merchants)",
  "event MandateRevoked(address indexed owner, address indexed agent)",
  "event InvoicePaid(bytes32 indexed invoiceId, address indexed merchant, address indexed payer, address agent, uint256 amount)",
  "error ZeroAmount()",
  "error ZeroAddress()",
  "error InvalidMandate()",
  "error NoMandate()",
  "error MandateExpired()",
  "error MerchantNotAllowed()",
  "error OverPerPaymentCap(uint256 amount, uint256 cap)",
  "error OverDailyCap(uint256 amount, uint256 remaining)",
  "error InvoiceAlreadyPaid(bytes32 invoiceId)",
];

export const TOKEN_ABI = [
  "function balanceOf(address) view returns (uint256)",
  "function allowance(address owner, address spender) view returns (uint256)",
  "function approve(address spender, uint256 amount) returns (bool)",
  "function symbol() view returns (string)",
  "function decimals() view returns (uint8)",
];
