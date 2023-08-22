// SPDX-License-Identifier: UNLICENSED
pragma solidity 0.8.16;

import {BaseTestWithConfiguredVerifierAndFeeManager} from "./BaseVerifierTest.t.sol";
import {IVerifier} from "../../interfaces/IVerifier.sol";
import {VerifierProxy} from "../../VerifierProxy.sol";
import {AccessControllerInterface} from "../../../interfaces/AccessControllerInterface.sol";
import {IERC165} from "../../../vendor/openzeppelin-solidity/v4.8.0/contracts/interfaces/IERC165.sol";
import {Common} from "../../../libraries/Common.sol";
import {FeeManager} from "../../dev/FeeManager.sol";

contract VerifierProxyInitializeVerifierTest is BaseTestWithConfiguredVerifierAndFeeManager {
  function test_setFeeManagerZeroAddress() public {
    vm.expectRevert(abi.encodeWithSelector(VerifierProxy.ZeroAddress.selector));
    s_verifierProxy.setFeeManager(FeeManager(address(0)));
  }
}
