// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title AgentPayRouter
/// @notice Invoice payments in one ERC-20 (tUSD8 on QDay Aevum), made by a person or by an
///         AI agent acting under a mandate the person granted.
///
///         Non-custodial: funds stay in the owner's wallet. The owner approves this contract
///         for the token and grants each agent key a mandate with a per-payment cap, a daily
///         cap, an expiry and, optionally, a merchant allowlist. A leaked or prompt-injected
///         agent key can therefore spend at most the daily cap, only to allowed merchants,
///         only until the mandate expires or the owner revokes it.
///
///         Every payment names an invoice id, and a merchant's invoice can be paid only once,
///         so a retrying agent cannot pay twice. Merchants learn about payments from the
///         `InvoicePaid` event, which the aevum-settle indexer follows to finality.
contract AgentPayRouter is ReentrancyGuard {
    using SafeERC20 for IERC20;

    struct Mandate {
        uint128 perPaymentCap;
        uint128 dailyCap;
        uint128 spentToday;
        uint64 day; // block.timestamp / 1 days of the last payment
        uint64 expiresAt; // 0 = no mandate
        uint32 epoch; // bumped on every grant and revoke; scopes the merchant allowlist
        bool merchantLocked; // true = only allowlisted merchants
    }

    IERC20 public immutable token;

    mapping(address owner => mapping(address agent => Mandate)) private _mandates;
    mapping(address owner => mapping(address agent => mapping(uint32 epoch => mapping(address merchant => bool))))
        private _allowed;
    mapping(address merchant => mapping(bytes32 invoiceId => bool)) public invoicePaid;

    event MandateGranted(
        address indexed owner,
        address indexed agent,
        uint128 perPaymentCap,
        uint128 dailyCap,
        uint64 expiresAt,
        address[] merchants
    );
    event MandateRevoked(address indexed owner, address indexed agent);
    event InvoicePaid(
        bytes32 indexed invoiceId, address indexed merchant, address indexed payer, address agent, uint256 amount
    );

    error ZeroAmount();
    error ZeroAddress();
    error InvalidMandate();
    error NoMandate();
    error MandateExpired();
    error MerchantNotAllowed();
    error OverPerPaymentCap(uint256 amount, uint256 cap);
    error OverDailyCap(uint256 amount, uint256 remaining);
    error InvoiceAlreadyPaid(bytes32 invoiceId);

    constructor(IERC20 token_) {
        if (address(token_) == address(0)) revert ZeroAddress();
        token = token_;
    }

    // ---------------------------------------------------------------- owner

    /// @notice Grants `agent` a mandate, replacing any earlier one (and its allowlist).
    /// @param merchants Allowed merchants; empty = any merchant.
    function grantMandate(
        address agent,
        uint128 perPaymentCap,
        uint128 dailyCap,
        uint64 expiresAt,
        address[] calldata merchants
    ) external {
        if (agent == address(0) || agent == msg.sender) revert ZeroAddress();
        if (perPaymentCap == 0 || dailyCap < perPaymentCap || expiresAt <= block.timestamp) {
            revert InvalidMandate();
        }
        Mandate storage m = _mandates[msg.sender][agent];
        uint32 epoch = m.epoch + 1;
        for (uint256 i = 0; i < merchants.length; i++) {
            if (merchants[i] == address(0)) revert ZeroAddress();
            _allowed[msg.sender][agent][epoch][merchants[i]] = true;
        }
        _mandates[msg.sender][agent] = Mandate({
            perPaymentCap: perPaymentCap,
            dailyCap: dailyCap,
            spentToday: 0,
            day: 0,
            expiresAt: expiresAt,
            epoch: epoch,
            merchantLocked: merchants.length > 0
        });
        emit MandateGranted(msg.sender, agent, perPaymentCap, dailyCap, expiresAt, merchants);
    }

    /// @notice Revokes `agent`'s mandate with immediate effect.
    function revokeMandate(address agent) external {
        Mandate storage m = _mandates[msg.sender][agent];
        if (m.expiresAt == 0) revert NoMandate();
        uint32 epoch = m.epoch + 1;
        delete _mandates[msg.sender][agent];
        _mandates[msg.sender][agent].epoch = epoch;
        emit MandateRevoked(msg.sender, agent);
    }

    // ---------------------------------------------------------------- pay

    /// @notice Pays an invoice from the caller's own balance (needs an allowance).
    function pay(bytes32 invoiceId, address merchant, uint256 amount) external nonReentrant {
        _settle(invoiceId, merchant, msg.sender, address(0), amount);
    }

    /// @notice Pays an invoice from `owner`'s balance under the caller's mandate.
    function payAsAgent(address owner, bytes32 invoiceId, address merchant, uint256 amount) external nonReentrant {
        Mandate storage m = _mandates[owner][msg.sender];
        if (m.expiresAt == 0) revert NoMandate();
        if (block.timestamp >= m.expiresAt) revert MandateExpired();
        if (m.merchantLocked && !_allowed[owner][msg.sender][m.epoch][merchant]) revert MerchantNotAllowed();
        if (amount > m.perPaymentCap) revert OverPerPaymentCap(amount, m.perPaymentCap);

        uint64 today = uint64(block.timestamp / 1 days);
        uint256 spent = m.day == today ? m.spentToday : 0;
        uint256 remaining = m.dailyCap - spent;
        if (amount > remaining) revert OverDailyCap(amount, remaining);
        m.day = today;
        m.spentToday = uint128(spent + amount);

        _settle(invoiceId, merchant, owner, msg.sender, amount);
    }

    function _settle(bytes32 invoiceId, address merchant, address payer, address agent, uint256 amount) private {
        if (amount == 0) revert ZeroAmount();
        if (merchant == address(0)) revert ZeroAddress();
        if (invoicePaid[merchant][invoiceId]) revert InvoiceAlreadyPaid(invoiceId);
        invoicePaid[merchant][invoiceId] = true;
        emit InvoicePaid(invoiceId, merchant, payer, agent, amount);
        token.safeTransferFrom(payer, merchant, amount);
    }

    // ---------------------------------------------------------------- views

    struct MandateView {
        uint128 perPaymentCap;
        uint128 dailyCap;
        uint128 remainingToday;
        uint64 expiresAt;
        bool active;
        bool merchantLocked;
    }

    function mandate(address owner, address agent) external view returns (MandateView memory v) {
        Mandate storage m = _mandates[owner][agent];
        uint256 spent = m.day == uint64(block.timestamp / 1 days) ? m.spentToday : 0;
        v.perPaymentCap = m.perPaymentCap;
        v.dailyCap = m.dailyCap;
        v.remainingToday = m.dailyCap > spent ? uint128(m.dailyCap - spent) : 0;
        v.expiresAt = m.expiresAt;
        v.active = m.expiresAt != 0 && block.timestamp < m.expiresAt;
        v.merchantLocked = m.merchantLocked;
    }

    function isMerchantAllowed(address owner, address agent, address merchant) external view returns (bool) {
        Mandate storage m = _mandates[owner][agent];
        if (m.expiresAt == 0) return false;
        return !m.merchantLocked || _allowed[owner][agent][m.epoch][merchant];
    }
}
